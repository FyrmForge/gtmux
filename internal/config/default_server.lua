-- gtmux server config. The client owns input (prefix + keybinds) and the
-- status bar (formats + colors), so almost everything lives in client.lua now.
-- The server-side options are below (all shown at their defaults).

-- Session auto-naming: the fmt template (must contain %d) for sessions created
-- by a bare `gtmux new`. Defaults to tmux's numeric "%d" (0, 1, 2, …):
-- gtmux.set_option("session_name", "%d")

-- Main pane size for the main-vertical / main-horizontal layouts (tmux's
-- main-pane-width / main-pane-height). The main pane takes this many cells;
-- the remaining panes share what's left. Defaults match tmux (80 / 24):
-- gtmux.set_option("main_pane_width", 80)
-- gtmux.set_option("main_pane_height", 24)

-- Multi-client grid policy when clients of different sizes share a session:
-- "latest" (default, follow the most recent client), "smallest", "largest", or
-- "manual" (grid set only by resize-window, ignoring client sizes).
-- gtmux.set_option("window_size", "latest")

-- Window/pane numbering base. tmux's compiled-in default is 0; gtmux ships 1
-- (like a common .tmux.conf). Set these to 0 for tmux's stock numbering.
-- (gtmux windows are always compact, so renumber-windows is inherently on.)
gtmux.set_option("base_index", 1)
gtmux.set_option("pane_base_index", 1)

-- Per-pane scrollback cap (tmux's history-limit; default 2000). Config-time
-- only — set here, not at runtime. gtmux ships a roomier 5000:
gtmux.set_option("history_limit", 5000)

-- How long a transient status message (run-shell output, command errors) stays
-- up, in ms. tmux's default is 750; gtmux ships a longer 3000 so output is
-- readable. Runtime-settable via set-option display-time.
gtmux.set_option("display_time", 3000)

-- How many past status messages show-messages keeps per session (tmux's
-- message-limit). Runtime-settable via set-option message-limit.
gtmux.set_option("message_limit", 1000)

-- Keep a pane after its process exits instead of closing it: "off" (default),
-- "on", or "failed" (keep only on a non-zero exit). A window option; override
-- per window at runtime with `setw remain-on-exit on`. respawn-pane revives a
-- dead pane.
gtmux.set_option("remain_on_exit", "off")

-- Shell command a copy-mode yank pipes the selection to on stdin (tmux's
-- copy-command). Empty (default) = no pipe; the system clipboard is still set
-- via OSC 52 and the paste buffer regardless. Runtime-settable with
-- `set-option copy-command`. Example: pipe to the Wayland clipboard —
-- gtmux.set_option("copy_command", "wl-copy")

-- Variables refreshed from an attaching client's environment into the session
-- on attach/reattach (tmux's update-environment), whitespace-separated. A
-- listed var missing from the client is unset from the session. Defaults to
-- tmux's list (DISPLAY SSH_AUTH_SOCK …); add your Wayland/DBus vars here so a
-- reattach picks up a fresh compositor session. Runtime-settable via
-- `set-option update-environment`.
-- gtmux.set_option("update_environment", "DISPLAY SSH_AUTH_SOCK WAYLAND_DISPLAY DBUS_SESSION_BUS_ADDRESS")

-- Send focus in/out escapes to a pane that requested them (DECSET 1004) as it
-- gains/loses the active-pane focus, so apps like vim see focus changes (tmux's
-- focus-events, default off). Runtime-settable via `set-option focus-events`.
-- gtmux.set_option("focus_events", true)

-- Let an app in a pane forward raw bytes to the outer terminal via an
-- ESC Ptmux;<payload> ESC \ DCS (tmux's allow-passthrough, default off). Off
-- strips and drops it. Runtime-settable via `set-option allow-passthrough`.
-- gtmux.set_option("allow_passthrough", true)

-- Exit the server process once its last session closes (tmux's exit-empty,
-- default on). Off keeps the daemon running with zero sessions.
-- gtmux.set_option("exit_empty", true)

-- Shell binary new panes run (tmux's default-shell); empty uses $SHELL then
-- /bin/sh. default-command runs via `shell -c` for a pane with no explicit
-- command; empty runs the shell as a login shell. Config-time (like
-- history-limit).
-- gtmux.set_option("default_shell", "/bin/zsh")
-- gtmux.set_option("default_command", "")

-- Which windows raise a monitor-activity / monitor-bell alert (tmux's
-- activity-action / bell-action): "any", "none", "current", or "other". tmux
-- defaults: activity-action "other" (only windows you're not viewing),
-- bell-action "any". Runtime-settable via set-option.
-- gtmux.set_option("activity_action", "other")
-- gtmux.set_option("bell_action", "any")

-- Hooks: run a command when an event fires. Supported events:
--   after-new-window, after-split-window, after-select-window,
--   after-select-pane, after-rename-window, after-kill-pane, after-kill-window,
--   after-resize-pane, pane-exited, session-created, session-closed,
--   session-renamed, client-attached, client-detached, client-session-changed,
--   alert-activity, alert-bell, alert-silence
-- Each command part is one arg; multi-word parts survive, quoting doesn't.
-- gtmux.set_hook("after-new-window", "select-layout tiled")
