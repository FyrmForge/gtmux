-- gtmux client config: chrome colors and toggles, applied per-attach.
-- Color values are names: black red green yellow blue magenta cyan white,
-- light_grey dark_grey, light_{red,green,yellow,blue,magenta,cyan}.

gtmux.options.mouse = true
gtmux.options.mode_keys = "vi" -- copy-mode keytable: "vi" or "emacs"
-- extended-keys (tmux's option, default off): while a pane app speaks the kitty
-- keyboard protocol, negotiate it with the outer terminal too, so disambiguated
-- keys (Ctrl+I vs Tab, Shift+Enter) reach the app. gtmux does kitty passthrough,
-- not tmux's modifyOtherKeys wire format. Set "on" to enable.
-- gtmux.options.extended_keys = "on"
-- Chars (besides whitespace) that bound copy-mode w/b/e words (tmux's
-- word-separators). Default is all ASCII punctuation, so foo.bar is three words.
-- gtmux.options.word_separators = "!\"#$%&'()*+,-./:;<=>?@[\\]^`{|}~"

-- status-style: the bar's default fg/bg/attrs as one tmux-style string
-- (comma-separated fg=/bg=/bold/reverse/italics). Partial — only what's listed.
gtmux.options.status_style = "fg=white,bg=dark_grey"

-- Multi-line status (tmux `status` 1..5): reserve N status rows. Line 1 is the
-- normal bar (status_left + window list + status_right); lines 2..5 each draw an
-- expanded full-width format (blank if unset). `off` isn't modeled (clamps to 1).
-- gtmux.options.status = "2"
-- gtmux.options.status_format_2 = "#{host}  #{clock}"

-- Status-bar layout and per-window-entry formats (all shown at their defaults):
-- gtmux.options.status_position = "bottom"  -- or "top"
-- gtmux.options.status_justify = "left"     -- "centre" | "right"
-- gtmux.options.window_status_separator = " "
-- gtmux.options.window_status_format = "#{window_index}:#{window_name}#{window_flags}"
-- gtmux.options.window_status_current_format = "#{window_index}:#{window_name}#{window_flags}"

gtmux.options.active_window_fg = "black"
gtmux.options.active_window_bg = "green"

gtmux.options.active_border_fg = "green"
gtmux.options.marked_border_fg = "magenta"
-- pane-border-style: the INACTIVE pane dividers' style (fg/bg/attr as one
-- tmux-style string). Active/marked borders keep active_border_fg/marked_border_fg.
-- gtmux.options.pane_border_style = "fg=dark_grey"
gtmux.options.fill_fg = "dark_grey"

gtmux.options.copy_cursor_fg = "black"
gtmux.options.copy_cursor_bg = "yellow"
gtmux.options.copy_selection_fg = "black"
gtmux.options.copy_selection_bg = "light_cyan"

-- Status bar format strings. The client owns and expands these. Variables:
--   #{host} #{session} #{window_name} #{git_branch} #{clock} #{pane_path}
--   #{pane_command}, plus #{?var,then,else} conditionals.
-- Shell output: #client(cmd) runs on this client's host, #server(cmd) runs on
-- the server; both cache their output for status_interval seconds.
gtmux.set_option("status_left", "[#{host}][#{session}]")
gtmux.set_option("status_right", "#{?git_branch,[git:#{git_branch}] ,}#{clock}")
gtmux.set_option("status_interval", "15")

-- Cap status-left / status-right to N cells (tmux status-left-length /
-- status-right-length). 0 = unlimited (gtmux default; tmux's 10/40 would cut
-- gtmux's longer default status-left).
-- gtmux.set_option("status_left_length", "0")
-- gtmux.set_option("status_right_length", "0")

-- Style (fg=/bg=/attr) of transient status messages + the command prompt
-- (tmux message-style). The copy-mode selection style is copy_selection_fg/bg.
-- gtmux.options.message_style = "fg=black,bg=yellow"

-- Prefix key and keybinds. The client owns all input: it tracks the prefix,
-- resolves the bound key to an action, and either sends that action to the
-- server or opens a local overlay (prompts/pickers). Edit freely.
gtmux.set_option("prefix", "C-b")

gtmux.bind("c", function() gtmux.new_window() end)
gtmux.bind("n", function() gtmux.next_window() end)
gtmux.bind("p", function() gtmux.prev_window() end)
gtmux.bind("%", function() gtmux.split_v() end)
gtmux.bind("\"", function() gtmux.split_h() end)
gtmux.bind("x", function() gtmux.kill_pane() end)
gtmux.bind("d", function() gtmux.detach() end)
gtmux.bind("q", function() gtmux.show_pane_numbers() end)
gtmux.bind("$", function() gtmux.rename_session_prompt() end)
gtmux.bind(",", function() gtmux.rename_window_prompt() end)
gtmux.bind("z", function() gtmux.zoom() end)
gtmux.bind(" ", function() gtmux.next_layout() end)       -- prefix+Space cycles presets
gtmux.bind("C-o", function() gtmux.rotate_window() end)   -- prefix+C-o rotates panes
gtmux.bind("{", function() gtmux.swap_pane("prev") end)
gtmux.bind("}", function() gtmux.swap_pane("next") end)
gtmux.bind("<", function() gtmux.swap_window("prev") end)
gtmux.bind(">", function() gtmux.swap_window("next") end)
gtmux.bind("!", function() gtmux.break_pane() end)
gtmux.bind("m", function() gtmux.mark_pane() end)
gtmux.bind("J", function() gtmux.join_marked() end)
gtmux.bind("w", function() gtmux.choose_tree() end)       -- session+window tree (tmux prefix+w)
gtmux.bind("W", function() gtmux.choose_window() end)     -- this session's windows only
gtmux.bind("s", function() gtmux.choose_session() end)
gtmux.bind(":", function() gtmux.command_prompt() end)
gtmux.bind("[", function() gtmux.enter_copy_mode() end)
gtmux.bind("]", function() gtmux.paste() end)

-- Splits (new panes already open in the active pane's cwd).
gtmux.bind("|", function() gtmux.split_v() end)
gtmux.bind("-", function() gtmux.split_h() end)

-- Directional pane resize, repeatable (tmux's `bind -r`): after the first
-- prefix+key, the bare key keeps resizing until the repeat window lapses.
gtmux.bind_repeat("h", function() gtmux.resize_pane("left", 2) end)
gtmux.bind_repeat("l", function() gtmux.resize_pane("right", 2) end)
gtmux.bind_repeat("k", function() gtmux.resize_pane("up", 2) end)
gtmux.bind_repeat("j", function() gtmux.resize_pane("down", 2) end)

-- Jump to a window by number: prefix then the digit. (tmux's no-prefix
-- C-1..9 isn't possible here — terminals emit no distinct byte for Ctrl+digit.)
for i = 1, 9 do
	gtmux.bind(tostring(i), function() gtmux.select_window(i) end)
end

-- Vim-aware pane navigation, no prefix (tmux's vim-split pattern). If the
-- focused pane runs vim the ctrl key is delivered to vim; otherwise it moves
-- between gtmux panes. C-\ selects the previously-active pane.
gtmux.bind_root("C-h", function() gtmux.select_pane_vim("left") end)
gtmux.bind_root("C-j", function() gtmux.select_pane_vim("down") end)
gtmux.bind_root("C-k", function() gtmux.select_pane_vim("up") end)
gtmux.bind_root("C-l", function() gtmux.select_pane_vim("right") end)
gtmux.bind_root("C-\\", function() gtmux.select_pane_vim("last") end)

-- Opt-in (these override gtmux's mark-pane `m` / join-marked `J`): bigger
-- resize steps and zoom on `m`, as in a typical tmux config.
-- gtmux.bind_repeat("H", function() gtmux.resize_pane("left", 10) end)
-- gtmux.bind_repeat("L", function() gtmux.resize_pane("right", 10) end)
-- gtmux.bind_repeat("K", function() gtmux.resize_pane("up", 10) end)
-- gtmux.bind_repeat("J", function() gtmux.resize_pane("down", 10) end)
-- gtmux.bind_repeat("m", function() gtmux.zoom() end)

-- Custom key tables (tmux bind -T / switch-client -T): key_table() switches the
-- client into a named table for the *next* key only (one-shot), so multi-key
-- sequences work. Example — prefix+g then n/p cycles windows:
-- gtmux.bind("g", function() gtmux.key_table("mygroup") end)
-- gtmux.bind_table("mygroup", "n", function() gtmux.next_window() end)
-- gtmux.bind_table("mygroup", "p", function() gtmux.prev_window() end)
