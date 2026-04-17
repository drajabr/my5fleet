// autotileWM is a minimal automatic tiling window manager for X11.
//
// It manages all top-level windows in an equal-area grid layout:
//
//	1 window  → fullscreen
//	2 windows → side by side  (1×2)
//	3 windows → 2 top + 1 bottom
//	4 windows → 2×2 grid
//	N windows → ceil(√N) columns, last row may have fewer (wider) cells
//
// Designed for headless VNC displays where every pixel counts.
// No config files, no IPC, no dependencies beyond X11.
//
// Usage:
//
//	autotilewm [-display :0] [-border 1] [-border-color 0x444444]
package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"
)

func main() {
	displayFlag := flag.String("display", "", "X display (default: $DISPLAY)")
	borderWidth := flag.Uint("border", 1, "border width in pixels (0 to disable)")
	borderColorHex := flag.String("border-color", "0x444444", "border color as hex (e.g. 0x444444)")
	flag.Parse()

	borderColor, err := strconv.ParseUint(*borderColorHex, 0, 32)
	if err != nil {
		log.Fatalf("bad -border-color %q: %v", *borderColorHex, err)
	}

	display := *displayFlag
	if display == "" {
		display = os.Getenv("DISPLAY")
	}

	conn, err := xgb.NewConnDisplay(display)
	if err != nil {
		log.Fatalf("cannot open display %q: %v", display, err)
	}
	defer conn.Close()

	setup := xproto.Setup(conn)
	screen := setup.DefaultScreen(conn)
	root := screen.Root

	// Become the window manager: grab SubstructureRedirect on root.
	// If another WM is running, this will fail with BadAccess.
	mask := xproto.EventMaskSubstructureRedirect |
		xproto.EventMaskSubstructureNotify |
		xproto.EventMaskStructureNotify
	err = xproto.ChangeWindowAttributesChecked(conn, root, xproto.CwEventMask,
		[]uint32{uint32(mask)}).Check()
	if err != nil {
		log.Fatalf("another window manager is running: %v", err)
	}

	// Enable RandR so we get notified when VNC resizes the screen.
	if err := randr.Init(conn); err != nil {
		log.Printf("warning: RandR init failed (resize detection disabled): %v", err)
	} else {
		randr.SelectInput(conn, root, randr.NotifyMaskScreenChange)
	}

	wm := &tileWM{
		conn:        conn,
		root:        root,
		screen:      screen,
		borderWidth: uint32(*borderWidth),
		borderColor: uint32(borderColor),
	}

	// Adopt any windows that already exist (e.g. if WM restarts).
	wm.adoptExisting()
	wm.retile()

	log.Printf("autotileWM running on %s (screen %dx%d, border %dpx)",
		display, screen.WidthInPixels, screen.HeightInPixels, *borderWidth)

	// Event loop — runs forever.
	for {
		ev, err := conn.WaitForEvent()
		if err != nil {
			log.Printf("X11 error: %v", err)
			continue
		}
		if ev == nil {
			log.Fatal("X connection closed")
		}
		wm.handle(ev)
	}
}

// ── Window manager state ───────────────────────────────────────────────────────

type tileWM struct {
	conn        *xgb.Conn
	root        xproto.Window
	screen      *xproto.ScreenInfo
	borderWidth uint32
	borderColor uint32
	windows     []xproto.Window
}

// adoptExisting queries all children of the root window and manages any that
// are in the IsViewable state. This handles the case where the WM is started
// after windows already exist.
func (wm *tileWM) adoptExisting() {
	tree, err := xproto.QueryTree(wm.conn, wm.root).Reply()
	if err != nil {
		return
	}
	for _, child := range tree.Children {
		attrs, err := xproto.GetWindowAttributes(wm.conn, child).Reply()
		if err != nil {
			continue
		}
		if attrs.MapState == xproto.MapStateViewable {
			wm.manage(child)
		}
	}
}

func (wm *tileWM) handle(ev xgb.Event) {
	switch e := ev.(type) {
	case xproto.MapRequestEvent:
		wm.onMapRequest(e)
	case xproto.UnmapNotifyEvent:
		wm.onUnmapNotify(e)
	case xproto.DestroyNotifyEvent:
		wm.onDestroyNotify(e)
	case xproto.ConfigureRequestEvent:
		wm.onConfigureRequest(e)
	case xproto.EnterNotifyEvent:
		wm.onEnterNotify(e)
	case randr.ScreenChangeNotifyEvent:
		wm.onScreenChange(e)
	}
}

// ── Event handlers ─────────────────────────────────────────────────────────────

func (wm *tileWM) onMapRequest(e xproto.MapRequestEvent) {
	xproto.MapWindow(wm.conn, e.Window)
	wm.manage(e.Window)
	wm.retile()
}

func (wm *tileWM) onUnmapNotify(e xproto.UnmapNotifyEvent) {
	if wm.remove(e.Window) {
		wm.retile()
	}
}

func (wm *tileWM) onDestroyNotify(e xproto.DestroyNotifyEvent) {
	if wm.remove(e.Window) {
		wm.retile()
	}
}

func (wm *tileWM) onConfigureRequest(e xproto.ConfigureRequestEvent) {
	// If the window is managed, ignore its requested geometry and retile.
	if wm.isManaged(e.Window) {
		wm.retile()
		return
	}
	// Unmanaged windows (e.g. override-redirect) get what they ask for.
	wm.forwardConfigure(e)
}

func (wm *tileWM) onEnterNotify(e xproto.EnterNotifyEvent) {
	xproto.SetInputFocus(wm.conn, xproto.InputFocusPointerRoot,
		e.Event, xproto.TimeCurrentTime)
}

func (wm *tileWM) onScreenChange(_ randr.ScreenChangeNotifyEvent) {
	// Re-read root geometry (Xvnc resized by noVNC).
	wm.retile()
}

// ── Window tracking ────────────────────────────────────────────────────────────

func (wm *tileWM) manage(win xproto.Window) {
	for _, w := range wm.windows {
		if w == win {
			return // already tracked
		}
	}
	// Subscribe to enter (focus-follows-pointer) and structure events.
	xproto.ChangeWindowAttributes(wm.conn, win, xproto.CwEventMask,
		[]uint32{uint32(xproto.EventMaskEnterWindow | xproto.EventMaskStructureNotify)})
	wm.windows = append(wm.windows, win)
}

func (wm *tileWM) remove(win xproto.Window) bool {
	for i, w := range wm.windows {
		if w == win {
			wm.windows = append(wm.windows[:i], wm.windows[i+1:]...)
			return true
		}
	}
	return false
}

func (wm *tileWM) isManaged(win xproto.Window) bool {
	for _, w := range wm.windows {
		if w == win {
			return true
		}
	}
	return false
}

// ── Grid tiling ────────────────────────────────────────────────────────────────

// retile recalculates the grid layout and moves/resizes all managed windows.
//
// Layout for N windows:
//
//	cols = ceil(sqrt(N))
//	rows = ceil(N / cols)
//
// The last row may have fewer windows, which are made wider to fill the row.
func (wm *tileWM) retile() {
	n := len(wm.windows)
	if n == 0 {
		return
	}

	// Current root (screen) geometry.
	geo, err := xproto.GetGeometry(wm.conn, xproto.Drawable(wm.root)).Reply()
	if err != nil {
		return
	}
	screenW := int(geo.Width)
	screenH := int(geo.Height)

	bw := int(wm.borderWidth)

	cols := int(math.Ceil(math.Sqrt(float64(n))))
	rows := int(math.Ceil(float64(n) / float64(cols)))

	idx := 0
	for row := 0; row < rows; row++ {
		// How many windows in this row? Last row gets the remainder.
		rowCols := cols
		remaining := n - idx
		if remaining < cols {
			rowCols = remaining
		}

		cellH := screenH / rows
		cellY := row * cellH
		// Last row absorbs any rounding leftover.
		if row == rows-1 {
			cellH = screenH - cellY
		}

		for col := 0; col < rowCols; col++ {
			cellW := screenW / rowCols
			cellX := col * cellW
			// Last column absorbs any rounding leftover.
			if col == rowCols-1 {
				cellW = screenW - cellX
			}

			// Subtract border from the window dimensions.
			// X11 border is drawn outside the window geometry,
			// so window width = cell width - 2*border.
			winX := cellX
			winY := cellY
			winW := cellW - 2*bw
			winH := cellH - 2*bw
			if winW < 1 {
				winW = 1
			}
			if winH < 1 {
				winH = 1
			}

			win := wm.windows[idx]
			xproto.ConfigureWindow(wm.conn, win,
				xproto.ConfigWindowX|xproto.ConfigWindowY|
					xproto.ConfigWindowWidth|xproto.ConfigWindowHeight|
					xproto.ConfigWindowBorderWidth,
				[]uint32{
					uint32(winX), uint32(winY),
					uint32(winW), uint32(winH),
					uint32(bw),
				})

			if bw > 0 {
				xproto.ChangeWindowAttributes(wm.conn, win,
					xproto.CwBorderPixel, []uint32{wm.borderColor})
			}

			idx++
		}
	}

	conn := wm.conn
	_ = conn // force a sync so all configure requests are flushed
}

// forwardConfigure sends the client's requested geometry to the X server
// for unmanaged (override-redirect) windows.
func (wm *tileWM) forwardConfigure(e xproto.ConfigureRequestEvent) {
	var mask uint16
	var values []uint32

	// Preserve the order required by X11 protocol: x, y, w, h, border, sibling, stack.
	if e.ValueMask&xproto.ConfigWindowX != 0 {
		mask |= xproto.ConfigWindowX
		values = append(values, uint32(e.X))
	}
	if e.ValueMask&xproto.ConfigWindowY != 0 {
		mask |= xproto.ConfigWindowY
		values = append(values, uint32(e.Y))
	}
	if e.ValueMask&xproto.ConfigWindowWidth != 0 {
		mask |= xproto.ConfigWindowWidth
		values = append(values, uint32(e.Width))
	}
	if e.ValueMask&xproto.ConfigWindowHeight != 0 {
		mask |= xproto.ConfigWindowHeight
		values = append(values, uint32(e.Height))
	}
	if e.ValueMask&xproto.ConfigWindowBorderWidth != 0 {
		mask |= xproto.ConfigWindowBorderWidth
		values = append(values, uint32(e.BorderWidth))
	}
	if e.ValueMask&xproto.ConfigWindowSibling != 0 {
		mask |= xproto.ConfigWindowSibling
		values = append(values, uint32(e.Sibling))
	}
	if e.ValueMask&xproto.ConfigWindowStackMode != 0 {
		mask |= xproto.ConfigWindowStackMode
		values = append(values, uint32(e.StackMode))
	}

	if mask != 0 {
		xproto.ConfigureWindow(wm.conn, e.Window, mask, values)
	}
}

func init() {
	log.SetFlags(log.Ltime)
	log.SetPrefix(fmt.Sprintf("[autotileWM:%d] ", os.Getpid()))
}
