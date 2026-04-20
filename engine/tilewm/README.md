# autotilingWM

Minimal X11 tiling window manager for headless/VNC worker displays.

Behavior:

- 1 window: full screen
- 2 windows: split in half
- N>=3: split the currently active tile to place the new window
- Closed window: its sibling expands into the reclaimed space
- Last remaining window: full screen again

No decorations, no panels, no keybindings, no floating mode.

Build:

```sh
make
```
