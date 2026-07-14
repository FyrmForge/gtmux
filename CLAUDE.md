# gtmux

Proof-of-concept Go port of tmux's client-server architecture: single binary
(`cmd/gtmux`), server subcommand runs a persistent daemon, client subcommand
attaches over a Unix socket. Per-session owner goroutine holds all state
(window/pane tree, emu grid, mode); PTY-reader and client-connection
goroutines feed it via channels, gob-encoded messages, no mutexes.

## Scratch files

Use `.tmp/` (gitignored) for throwaway test scripts/binaries, not the
external session scratchpad. It's already inside the working directory so it
doesn't trigger permission prompts.

## Testing the client interactively

The client needs a real TTY (raw terminal mode) — it can't be exercised
through a piped Bash stdin. Test setup, in the tmux session `ff-gtmux`:

- pane `%29` is the Claude Code pane
- pane `%31` is reserved empty, for driving/observing the gtmux client live

Run the server via Bash in the background:
```
gtmux server <session> > .tmp/server.log 2>&1 & disown
```
Drive the client in pane `%31`:
```
tmux send-keys -t %31 "gtmux attach <session>" Enter
tmux send-keys -t %31 "some command" Enter
tmux capture-pane -t %31 -p          # read back as text
```
Only kill/recreate `%31` if the shell itself breaks — otherwise leave the
server and client attached between test rounds. Pane IDs can change if panes
are recreated; re-check with:
```
tmux list-panes -t ff-gtmux -F '#{pane_index} #{pane_id} #{pane_current_command}'
```

### Actual visual screenshots

Compositor is Hyprland/Wayland, `grim` is installed — this gives real
rendered pixels, not just text/escape codes. The wezterm window titled
`ff-gtmux` is at `7,24` size `1906x1049` (recompute if the window moves or
resizes):
```
hyprctl clients | awk '/^Window/{w=$0} /ff-gtmux/{print w; found=1} found && /^\t(at|size):/{print}'
```
Crop to just the test pane (`%31`, right half) and read it as an image:
```
grim -g "962,24 951x1049" .tmp/pane.png
```
