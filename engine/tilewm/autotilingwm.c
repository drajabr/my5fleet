#include <X11/Xatom.h>
#include <X11/Xlib.h>
#include <X11/Xutil.h>

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef struct Node {
    Window win;
    struct Node *parent;
    struct Node *left;
    struct Node *right;
    int split_vertical;
} Node;

typedef struct {
    unsigned long flags;
    unsigned long functions;
    unsigned long decorations;
    long input_mode;
    unsigned long status;
} MotifWmHints;

static Display *dpy;
static int screen_num;
static Window root;

static Node *tree_root;
static Node *active_leaf;
static int managed_count;

static Atom atom_net_supporting_wm_check;
static Atom atom_net_wm_name;
static Atom atom_utf8_string;
static Atom atom_net_active_window;
static Atom atom_motif_wm_hints;
static Atom atom_net_frame_extents;
static Atom atom_net_wm_window_type;
static Atom atom_net_wm_window_type_dialog;
static Atom atom_net_wm_window_type_utility;
static Atom atom_net_wm_window_type_toolbar;
static Atom atom_net_wm_window_type_menu;
static Atom atom_net_wm_window_type_popup_menu;
static Atom atom_net_wm_window_type_dropdown_menu;
static Atom atom_net_wm_window_type_tooltip;

static unsigned int tile_border_width = 1;
static unsigned long tile_border_pixel;

static Window wm_check_win;

static Node *make_leaf(Window w) {
    Node *n = calloc(1, sizeof(Node));
    if (!n) {
        fprintf(stderr, "autotilingwm: out of memory\n");
        exit(1);
    }
    n->win = w;
    return n;
}

static int is_leaf(const Node *n) {
    return n && !n->left && !n->right;
}

static Node *first_leaf(Node *n) {
    if (!n) {
        return NULL;
    }
    if (is_leaf(n)) {
        return n;
    }
    Node *l = first_leaf(n->left);
    if (l) {
        return l;
    }
    return first_leaf(n->right);
}

static Node *find_node(Node *n, Window w) {
    if (!n) {
        return NULL;
    }
    if (is_leaf(n) && n->win == w) {
        return n;
    }
    Node *l = find_node(n->left, w);
    if (l) {
        return l;
    }
    return find_node(n->right, w);
}

static void focus_window(Window w) {
    XSetInputFocus(dpy, w, RevertToPointerRoot, CurrentTime);
    XChangeProperty(dpy, root, atom_net_active_window, XA_WINDOW, 32, PropModeReplace, (unsigned char *)&w, 1);
}

static void set_window_no_decor(Window w) {
    MotifWmHints hints;
    memset(&hints, 0, sizeof(hints));
    hints.flags = 1UL << 1;
    hints.decorations = 0;
    XChangeProperty(
        dpy,
        w,
        atom_motif_wm_hints,
        atom_motif_wm_hints,
        32,
        PropModeReplace,
        (unsigned char *)&hints,
        sizeof(hints) / sizeof(long)
    );

    long extents[4] = {0, 0, 0, 0};
    XChangeProperty(
        dpy,
        w,
        atom_net_frame_extents,
        XA_CARDINAL,
        32,
        PropModeReplace,
        (unsigned char *)extents,
        4
    );
}

static int window_has_type(Window w, Atom wanted_type) {
    Atom actual_type;
    int actual_format;
    unsigned long nitems;
    unsigned long bytes_after;
    unsigned char *data = NULL;

    int ok = XGetWindowProperty(
        dpy,
        w,
        atom_net_wm_window_type,
        0,
        32,
        False,
        XA_ATOM,
        &actual_type,
        &actual_format,
        &nitems,
        &bytes_after,
        &data
    );
    if (ok != Success || !data) {
        return 0;
    }

    Atom *types = (Atom *)data;
    int found = 0;
    for (unsigned long i = 0; i < nitems; i++) {
        if (types[i] == wanted_type) {
            found = 1;
            break;
        }
    }

    XFree(data);
    return found;
}

static void layout_node(Node *n, int x, int y, int w, int h) {
    if (!n) {
        return;
    }
    if (w < 1) {
        w = 1;
    }
    if (h < 1) {
        h = 1;
    }

    if (is_leaf(n)) {
        unsigned int bw = (managed_count > 1) ? tile_border_width : 0;
        XSetWindowBorderWidth(dpy, n->win, bw);
        XSetWindowBorder(dpy, n->win, tile_border_pixel);
        XMoveResizeWindow(dpy, n->win, x, y, (unsigned int)w, (unsigned int)h);
        return;
    }

    if (n->split_vertical) {
        int left_w = w / 2;
        int right_w = w - left_w;
        layout_node(n->left, x, y, left_w, h);
        layout_node(n->right, x + left_w, y, right_w, h);
    } else {
        int top_h = h / 2;
        int bottom_h = h - top_h;
        layout_node(n->left, x, y, w, top_h);
        layout_node(n->right, x, y + top_h, w, bottom_h);
    }
}

static void arrange(void) {
    if (!tree_root) {
        return;
    }
    int sw = DisplayWidth(dpy, screen_num);
    int sh = DisplayHeight(dpy, screen_num);
    layout_node(tree_root, 0, 0, sw, sh);
    XFlush(dpy);
}

static void locate_node_rect(Node *cur, Node *target, int x, int y, int w, int h, int *out_x, int *out_y, int *out_w, int *out_h) {
    if (!cur) {
        return;
    }
    if (cur == target) {
        *out_x = x;
        *out_y = y;
        *out_w = w;
        *out_h = h;
        return;
    }

    if (is_leaf(cur)) {
        return;
    }

    if (cur->split_vertical) {
        int left_w = w / 2;
        int right_w = w - left_w;
        locate_node_rect(cur->left, target, x, y, left_w, h, out_x, out_y, out_w, out_h);
        locate_node_rect(cur->right, target, x + left_w, y, right_w, h, out_x, out_y, out_w, out_h);
    } else {
        int top_h = h / 2;
        int bottom_h = h - top_h;
        locate_node_rect(cur->left, target, x, y, w, top_h, out_x, out_y, out_w, out_h);
        locate_node_rect(cur->right, target, x, y + top_h, w, bottom_h, out_x, out_y, out_w, out_h);
    }
}

static int node_alive(Node *n) {
    return n && find_node(tree_root, n->win) == n;
}

static void insert_window(Window w) {
    if (!tree_root) {
        tree_root = make_leaf(w);
        active_leaf = tree_root;
        managed_count = 1;
        arrange();
        return;
    }

    Node *target = active_leaf;
    if (!node_alive(target)) {
        target = first_leaf(tree_root);
    }
    if (!target) {
        target = tree_root;
    }

    int sw = DisplayWidth(dpy, screen_num);
    int sh = DisplayHeight(dpy, screen_num);
    int tx = 0;
    int ty = 0;
    int tw = sw;
    int th = sh;
    locate_node_rect(tree_root, target, 0, 0, sw, sh, &tx, &ty, &tw, &th);

    Node *new_leaf = make_leaf(w);
    Node *parent = calloc(1, sizeof(Node));
    if (!parent) {
        fprintf(stderr, "autotilingwm: out of memory\n");
        exit(1);
    }

    parent->left = target;
    parent->right = new_leaf;
    parent->split_vertical = (tw >= th);
    parent->parent = target->parent;

    if (!target->parent) {
        tree_root = parent;
    } else if (target->parent->left == target) {
        target->parent->left = parent;
    } else {
        target->parent->right = parent;
    }

    target->parent = parent;
    new_leaf->parent = parent;

    active_leaf = new_leaf;
    managed_count++;
    arrange();
}

static void free_subtree(Node *n) {
    if (!n) {
        return;
    }
    free_subtree(n->left);
    free_subtree(n->right);
    free(n);
}

static void remove_window(Window w) {
    Node *n = find_node(tree_root, w);
    if (!n) {
        return;
    }

    if (n == tree_root) {
        free(n);
        tree_root = NULL;
        active_leaf = NULL;
        managed_count = 0;
        return;
    }

    Node *parent = n->parent;
    Node *sibling = (parent->left == n) ? parent->right : parent->left;
    Node *grand = parent->parent;

    if (!grand) {
        tree_root = sibling;
        sibling->parent = NULL;
    } else {
        if (grand->left == parent) {
            grand->left = sibling;
        } else {
            grand->right = sibling;
        }
        sibling->parent = grand;
    }

    if (active_leaf == n || !node_alive(active_leaf)) {
        active_leaf = first_leaf(tree_root);
    }

    free(n);
    free(parent);
    managed_count--;
    if (managed_count < 0) {
        managed_count = 0;
    }
    arrange();
}

static int should_manage(Window w) {
    XWindowAttributes wa;
    Window transient_for;

    if (!XGetWindowAttributes(dpy, w, &wa)) {
        return 0;
    }
    if (wa.override_redirect) {
        return 0;
    }
    if (wa.class == InputOnly) {
        return 0;
    }
    if (wa.width <= 1 && wa.height <= 1) {
        return 0;
    }

    if (XGetTransientForHint(dpy, w, &transient_for)) {
        return 0;
    }

    if (window_has_type(w, atom_net_wm_window_type_dialog) ||
        window_has_type(w, atom_net_wm_window_type_utility) ||
        window_has_type(w, atom_net_wm_window_type_toolbar) ||
        window_has_type(w, atom_net_wm_window_type_menu) ||
        window_has_type(w, atom_net_wm_window_type_popup_menu) ||
        window_has_type(w, atom_net_wm_window_type_dropdown_menu) ||
        window_has_type(w, atom_net_wm_window_type_tooltip)) {
        return 0;
    }

    return 1;
}

static void manage_window(Window w, int map_if_needed) {
    if (!should_manage(w)) {
        // Non-tiled windows (dialogs, transient popups): still suppress Wine
        // decorations by setting _NET_FRAME_EXTENTS=0 before mapping.
        // Wine checks this at MapWindow time; without it, Wine draws its own
        // title bar even with Decorated=N in the registry.
        long extents[4] = {0, 0, 0, 0};
        XChangeProperty(dpy, w, atom_net_frame_extents, XA_CARDINAL, 32,
                        PropModeReplace, (unsigned char *)extents, 4);
        if (map_if_needed) {
            XMapWindow(dpy, w);
        }
        return;
    }
    if (find_node(tree_root, w)) {
        return;
    }

    XSelectInput(dpy, w, StructureNotifyMask | EnterWindowMask | PropertyChangeMask | ButtonPressMask);
    set_window_no_decor(w);
    if (map_if_needed) {
        XMapWindow(dpy, w);
    }

    insert_window(w);
    focus_window(w);
}

static void setup_wm_check(void) {
    wm_check_win = XCreateSimpleWindow(dpy, root, 0, 0, 1, 1, 0, 0, 0);

    XChangeProperty(
        dpy,
        root,
        atom_net_supporting_wm_check,
        XA_WINDOW,
        32,
        PropModeReplace,
        (unsigned char *)&wm_check_win,
        1
    );
    XChangeProperty(
        dpy,
        wm_check_win,
        atom_net_supporting_wm_check,
        XA_WINDOW,
        32,
        PropModeReplace,
        (unsigned char *)&wm_check_win,
        1
    );

    const char name[] = "autotilingWM";
    XChangeProperty(
        dpy,
        wm_check_win,
        atom_net_wm_name,
        atom_utf8_string,
        8,
        PropModeReplace,
        (const unsigned char *)name,
        (int)strlen(name)
    );
}

static void scan_existing_windows(void) {
    Window root_ret;
    Window parent_ret;
    Window *children = NULL;
    unsigned int nchildren = 0;

    if (!XQueryTree(dpy, root, &root_ret, &parent_ret, &children, &nchildren)) {
        return;
    }

    for (unsigned int i = 0; i < nchildren; i++) {
        XWindowAttributes wa;
        if (!XGetWindowAttributes(dpy, children[i], &wa)) {
            continue;
        }
        if (wa.map_state == IsViewable) {
            manage_window(children[i], 0);
        }
    }

    if (children) {
        XFree(children);
    }
}

static int on_xerror(Display *display_unused, XErrorEvent *e) {
    (void)display_unused;
    if (e->error_code == BadAccess) {
        fprintf(stderr, "autotilingwm: another window manager is already running\n");
        exit(1);
    }
    return 0;
}

int main(int argc, char **argv) {
    const char *display_name = NULL;

    for (int i = 1; i < argc; i++) {
        if (strcmp(argv[i], "-display") == 0 && i + 1 < argc) {
            display_name = argv[++i];
        }
    }

    dpy = XOpenDisplay(display_name);
    if (!dpy) {
        fprintf(stderr, "autotilingwm: cannot open display\n");
        return 1;
    }

    XSetErrorHandler(on_xerror);

    screen_num = DefaultScreen(dpy);
    root = RootWindow(dpy, screen_num);

    atom_net_supporting_wm_check = XInternAtom(dpy, "_NET_SUPPORTING_WM_CHECK", False);
    atom_net_wm_name = XInternAtom(dpy, "_NET_WM_NAME", False);
    atom_utf8_string = XInternAtom(dpy, "UTF8_STRING", False);
    atom_net_active_window = XInternAtom(dpy, "_NET_ACTIVE_WINDOW", False);
    atom_motif_wm_hints = XInternAtom(dpy, "_MOTIF_WM_HINTS", False);
    atom_net_frame_extents = XInternAtom(dpy, "_NET_FRAME_EXTENTS", False);
    atom_net_wm_window_type = XInternAtom(dpy, "_NET_WM_WINDOW_TYPE", False);
    atom_net_wm_window_type_dialog = XInternAtom(dpy, "_NET_WM_WINDOW_TYPE_DIALOG", False);
    atom_net_wm_window_type_utility = XInternAtom(dpy, "_NET_WM_WINDOW_TYPE_UTILITY", False);
    atom_net_wm_window_type_toolbar = XInternAtom(dpy, "_NET_WM_WINDOW_TYPE_TOOLBAR", False);
    atom_net_wm_window_type_menu = XInternAtom(dpy, "_NET_WM_WINDOW_TYPE_MENU", False);
    atom_net_wm_window_type_popup_menu = XInternAtom(dpy, "_NET_WM_WINDOW_TYPE_POPUP_MENU", False);
    atom_net_wm_window_type_dropdown_menu = XInternAtom(dpy, "_NET_WM_WINDOW_TYPE_DROPDOWN_MENU", False);
    atom_net_wm_window_type_tooltip = XInternAtom(dpy, "_NET_WM_WINDOW_TYPE_TOOLTIP", False);

    Colormap cmap = DefaultColormap(dpy, screen_num);
    XColor color;
    XColor exact;
    if (XAllocNamedColor(dpy, cmap, "#2b3040", &color, &exact)) {
        tile_border_pixel = color.pixel;
    } else {
        tile_border_pixel = WhitePixel(dpy, screen_num);
    }

    XSelectInput(dpy, root, SubstructureRedirectMask | SubstructureNotifyMask | StructureNotifyMask);

    setup_wm_check();
    scan_existing_windows();
    arrange();

    for (;;) {
        XEvent ev;
        XNextEvent(dpy, &ev);

        switch (ev.type) {
            case MapRequest: {
                manage_window(ev.xmaprequest.window, 1);
                break;
            }
            case ConfigureRequest: {
                XConfigureRequestEvent *cr = &ev.xconfigurerequest;
                if (find_node(tree_root, cr->window)) {
                    arrange();
                } else {
                    XWindowChanges wc;
                    wc.x = cr->x;
                    wc.y = cr->y;
                    wc.width = cr->width;
                    wc.height = cr->height;
                    wc.border_width = 0;
                    wc.sibling = cr->above;
                    wc.stack_mode = cr->detail;
                    XConfigureWindow(dpy, cr->window, cr->value_mask, &wc);
                }
                break;
            }
            case ConfigureNotify: {
                if (ev.xconfigure.window == root) {
                    arrange();
                }
                break;
            }
            case UnmapNotify: {
                remove_window(ev.xunmap.window);
                break;
            }
            case DestroyNotify: {
                remove_window(ev.xdestroywindow.window);
                break;
            }
            case EnterNotify: {
                Node *n = find_node(tree_root, ev.xcrossing.window);
                if (n) {
                    active_leaf = n;
                    focus_window(n->win);
                }
                break;
            }
            case ButtonPress: {
                Node *n = find_node(tree_root, ev.xbutton.window);
                if (n) {
                    active_leaf = n;
                    focus_window(n->win);
                }
                break;
            }
            default:
                break;
        }
    }

    free_subtree(tree_root);
    XCloseDisplay(dpy);
    return 0;
}
