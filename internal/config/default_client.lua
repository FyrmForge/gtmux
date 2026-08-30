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
-- pane-borders: "simple" (default; straight │/─ like tmux) | "joined" (box-drawing
-- junctions ┼├┤┬┴ where dividers cross) | "framed" (every pane fully enclosed).
-- gtmux.options.pane_borders = "joined"
gtmux.options.fill_fg = "dark_grey"

gtmux.options.copy_cursor_fg = "black"
gtmux.options.copy_cursor_bg = "yellow"
gtmux.options.copy_selection_fg = "black"
gtmux.options.copy_selection_bg = "light_cyan"

-- status_interval: refresh cadence (structural) — how often the bar re-renders
-- and #client/#server shell output is cached, in seconds.
gtmux.options.status_interval = 15

-- Status bar: a component (dock="status"). gtmux composes the bar in Lua rather
-- than from format-string options — reshape it by editing this. It reads
-- gtmux.windows()/expand()/context() and paints:
--   left = [host][session], a clickable window list (click selects; w.flags is
--   tmux's * # ! ~ Z), right = git branch + clock. #{...} still expands via
--   gtmux.expand; the window list is a child per window (its own on_click).
gtmux.widget{ dock = "status", component = function(props, ui)
  local BAR = "fg=white,bg=dark_grey" -- inactive / bar background (status-style)
  local CUR = "fg=black,bg=green"      -- active window (active-window colors)
  ui:fill(BAR)
  local left = "[" .. gtmux.expand("#{host}") .. "][" .. gtmux.expand("#{session}") .. "]"
  ui:text(0, 0, left, BAR)
  local x = #left + 1
  for i, w in ipairs(gtmux.windows()) do
    if i > 1 then x = x + 1 end -- separator
    local label = w.index .. ":" .. w.name .. w.flags
    ui:child(x, 0, #label, 1, function(p, cell)
      cell:text(0, 0, p.label, p.style)
      cell:on_click(function() gtmux.select_window(p.idx) end)
    end, { label = label, style = (w.active and CUR or BAR), idx = w.index })
    x = x + #label
  end
  local right = gtmux.expand("#{?git_branch,[git:#{git_branch}] ,}") .. gtmux.expand("#{clock}")
  ui:text(ui.w - #right, 0, right, BAR)
end }

-- Cap status-left / status-right to N cells (tmux status-left-length /
-- status-right-length). 0 = unlimited (gtmux default; tmux's 10/40 would cut
-- gtmux's longer default status-left).
-- gtmux.options.status_left_length = 0
-- gtmux.options.status_right_length = 0

-- Style (fg=/bg=/attr) of transient status messages + the command prompt
-- (tmux message-style). The copy-mode selection style is copy_selection_fg/bg.
-- gtmux.options.message_style = "fg=black,bg=yellow"

-- Widgets: gtmux can composite custom overlay/dock elements (status bars on any
-- edge, live data panels, clickable UIs driven by Lua queries, and 2D drawing —
-- boxes/borders/separators via a canvas `draw` fn). Off by default; not a tmux
-- feature. See gtmux.widget / gtmux.sessions / find_panes / draw docs.
--
-- A dock can also take keyboard focus: give it `on_key` plus `focus = "nav"`
-- (pane navigation at the window edge steps into it), "bind"
-- (gtmux.focus_dock(name), needs `name = ...`), or "both". While focused every
-- key goes to on_key — ui:state().focused is true so the component can show a
-- highlight — until on_key calls ui:close() (e.g. on Escape, or the nav key
-- pointing back at the panes). Example skeleton for a left session list:
-- gtmux.widget{ dock = "left", size = 20, name = "sessions", focus = "both",
--   component = function(props, ui)
--     local st = ui:state(); st.sel = st.sel or 1
--     for i, s in ipairs(gtmux.sessions()) do
--       local style = (st.focused and i == st.sel) and "fg=black,bg=yellow" or ""
--       ui:text(0, i - 1, s.name, style)
--     end
--   end,
--   on_key = function(key, ui)
--     local st = ui:state(); st.sel = st.sel or 1
--     if key == "j" or key == "Down" then st.sel = st.sel + 1
--     elseif key == "k" or key == "Up" then st.sel = math.max(1, st.sel - 1)
--     elseif key == "Enter" then
--       local s = gtmux.sessions()[st.sel]; if s then gtmux.switch_session(s.name) end
--     elseif key == "Escape" or key == "Right" or key == "l" then ui:close()
--     end
--   end }
--
-- Responsive: `min_cols = 120` on a dock auto-hides it when the client is
-- narrower (a phone attach sheds chrome; the window gets the columns back).
-- Bind gtmux.toggle_dock(name) to show/hide it manually — the toggle
-- overrides the breakpoint until toggled again:
-- gtmux.bind("b", function() gtmux.toggle_dock("sessions") end)
--
-- Small-screen maximize: below cols_below the client keeps the active pane
-- zoomed (one pane at a time, phone-style); crossing back above unzooms.
-- next_pane/prev_pane cycle panes KEEPING the zoom. Zoom is session state, so
-- other attached clients see it too. Pair with min_cols on your docks.
-- gtmux.responsive{ cols_below = 90 }
-- gtmux.bind("Tab", gtmux.next_pane)

-- Prefix key and keybinds. The client owns all input: it tracks the prefix,
-- resolves the bound key to an action, and either sends that action to the
-- server or opens a local overlay (prompts/pickers). Edit freely.
gtmux.options.prefix = "C-b"

-- Nullary verbs pass straight to bind (no wrapper closure); only binds that pass
-- an argument or run several statements keep a `function() ... end`.
gtmux.bind("c", gtmux.new_window)
gtmux.bind("n", gtmux.next_window)
gtmux.bind("p", gtmux.prev_window)
gtmux.bind("%", gtmux.split_right) -- new pane to the right
gtmux.bind("\"", gtmux.split_down) -- new pane below
gtmux.bind("x", gtmux.kill_pane)
gtmux.bind("d", gtmux.detach)
gtmux.bind("q", gtmux.show_pane_numbers)
-- prompt: a one-line text-input widget on the status/message line (position =
-- "status" — placeable elsewhere via gtmux.open). Type to edit, Enter submits
-- the text to on_submit, Esc cancels. Basis for rename (and command-prompt).
local function prompt(label, initial, on_submit)
  gtmux.open{
    position = "status",
    component = function(props, ui)
      local st = ui:state(); if st.text == nil then st.text = initial or "" end
      ui:fill("fg=black,bg=yellow") -- message-style
      ui:text(0, 0, "(" .. label .. ") " .. st.text, "fg=black,bg=yellow")
    end,
    on_key = function(key, ui)
      local st = ui:state(); if st.text == nil then st.text = initial or "" end
      if key == "Enter" then on_submit(st.text); ui:close()
      elseif key == "Escape" then ui:close()
      elseif key == "BSpace" then st.text = st.text:sub(1, -2)
      elseif #key == 1 then st.text = st.text .. key
      end
    end,
  }
end

gtmux.bind("$", function()
  prompt("rename-session", gtmux.expand("#{session}"), function(t) gtmux.rename_session(t) end)
end)
gtmux.bind(",", function()
  prompt("rename-window", gtmux.expand("#{window_name}"), function(t) gtmux.rename_window(t) end)
end)
gtmux.bind("z", gtmux.zoom)
-- prefix+e toggles prose highlighting in agent panes (see gtmux.agents below):
-- "syntax highlighting for English" — function words dim, Capitalized bold,
-- numbers/quotes/`code` colored. A readability/dyslexia aid for agent output.
gtmux.bind("e", gtmux.prose_highlight)
gtmux.bind(" ", gtmux.next_layout)     -- prefix+Space cycles presets
gtmux.bind("C-o", gtmux.rotate_window) -- prefix+C-o rotates panes
gtmux.bind("{", function() gtmux.swap_pane("prev") end)
gtmux.bind("}", function() gtmux.swap_pane("next") end)
gtmux.bind("<", function() gtmux.swap_window("prev") end)
gtmux.bind(">", function() gtmux.swap_window("next") end)
gtmux.bind("!", gtmux.break_pane)
gtmux.bind("m", gtmux.mark_pane)
gtmux.bind("J", gtmux.join_marked)
-- choose-tree (prefix+w): a cross-session tree (every session's windows), type
-- to filter, arrows to move, Enter switches to that session AND focuses that
-- window. Composed in Lua over gtmux.sessions()/windows() + switch_session(name,idx).
gtmux.bind("w", function()
  -- Recomputed on demand (not stashed): a multi-key chunk like "WTARGET<Enter>"
  -- updates the filter then selects, so on_key must see the CURRENT filter's rows.
  local function tree_rows(filter)
    local rows = {}
    for _, s in ipairs(gtmux.sessions()) do
      for _, w in ipairs(gtmux.windows({ session = s.name })) do
        local label = s.name .. ":" .. w.index .. ":" .. w.name
        if filter == "" or label:find(filter, 1, true) then
          rows[#rows + 1] = { session = s.name, index = w.index, label = label }
        end
      end
    end
    return rows
  end
  gtmux.open{
    width = 46, height = 16,
    component = function(props, ui)
      local st = ui:state(); st.sel = st.sel or 1; st.filter = st.filter or ""
      ui:fill("bg=black")
      ui:box(0, 0, ui.w, ui.h, { style = "fg=cyan", title = "choose tree" })
      ui:text(2, 1, "filter: " .. st.filter, "fg=yellow")
      local rows = tree_rows(st.filter)
      if st.sel > #rows then st.sel = math.max(1, #rows) end
      for i, r in ipairs(rows) do
        local y = i + 2
        if y < ui.h - 1 then
          ui:text(2, y, (i == st.sel and "> " or "  ") .. r.label,
            (i == st.sel) and "fg=black,bg=green" or "fg=white")
        end
      end
    end,
    on_key = function(key, ui)
      local st = ui:state(); st.sel = st.sel or 1; st.filter = st.filter or ""
      if key == "Down" then st.sel = math.min(#tree_rows(st.filter), st.sel + 1)
      elseif key == "Up" then st.sel = math.max(1, st.sel - 1)
      elseif key == "Enter" then
        local r = tree_rows(st.filter)[st.sel]
        if r then gtmux.switch_session(r.session, r.index) end
        ui:close()
      elseif key == "Escape" then ui:close()
      elseif key == "BSpace" then st.filter = st.filter:sub(1, -2); st.sel = 1
      elseif #key == 1 then st.filter = st.filter .. key; st.sel = 1
      end
    end,
  }
end)
-- Pickers are modal components (built on gtmux.open) now, not server overlays —
-- composed in Lua so you can restyle/extend them. simple_picker is a reusable
-- list picker: j/k or arrows move, Enter selects, Esc/q cancels. rows_fn returns
-- the items, label_fn the display string, select_fn acts on the chosen one.
local function simple_picker(title, rows_fn, label_fn, select_fn)
  gtmux.open{
    width = 34, height = 12,
    component = function(props, ui)
      local st = ui:state(); st.sel = st.sel or 1
      ui:fill("bg=black")
      ui:box(0, 0, ui.w, ui.h, { style = "fg=cyan", title = title })
      local rows = rows_fn()
      if st.sel > #rows then st.sel = math.max(1, #rows) end
      for i, it in ipairs(rows) do
        if i < ui.h - 1 then
          ui:text(2, i, (i == st.sel and "> " or "  ") .. label_fn(it),
            (i == st.sel) and "fg=black,bg=green" or "fg=white")
        end
      end
    end,
    on_key = function(key, ui)
      local st = ui:state(); st.sel = st.sel or 1
      local rows = rows_fn()
      if key == "Down" or key == "j" then st.sel = math.min(#rows, st.sel + 1)
      elseif key == "Up" or key == "k" then st.sel = math.max(1, st.sel - 1)
      elseif key == "Enter" then
        if rows[st.sel] then select_fn(rows[st.sel]) end
        ui:close()
      elseif key == "Escape" or key == "q" then ui:close()
      end
    end,
  }
end

gtmux.bind("s", function()
  simple_picker("choose session", gtmux.sessions,
    function(s) return s.name .. (s.attached and " (attached)" or "") end,
    function(s) gtmux.switch_session(s.name) end)
end)
gtmux.bind("W", function()
  simple_picker("choose window", gtmux.windows,
    function(w) return w.index .. ":" .. w.name .. w.flags end,
    function(w) gtmux.select_window(w.index) end)
end)

-- display-menu as a component (overrides the server verb): (title, label1, cmd1,
-- label2, cmd2, ...) — a modal list of labels; Enter runs the selected command
-- via run_command. Composed over simple_picker.
function gtmux.display_menu(title, ...)
  local args = { ... }
  local items = {}
  for i = 1, #args, 2 do
    items[#items + 1] = { label = args[i], cmd = args[i + 1] }
  end
  simple_picker(title, function() return items end,
    function(it) return it.label end,
    function(it) gtmux.run_command(it.cmd) end)
end

-- choose-buffer as a component (overrides the server verb): pick a paste buffer,
-- Enter pastes it into the active pane. prefix+= opens it (tmux-faithful).
function gtmux.choose_buffer()
  simple_picker("choose buffer", gtmux.buffers,
    function(b) return b.name .. ": " .. b.preview end,
    function(b) gtmux.paste_buffer(b.name) end)
end
gtmux.bind("=", gtmux.choose_buffer)

-- clock (prefix+t): a live clock overlay; any key dismisses. Re-renders on the
-- status tick, so #{clock} stays current.
gtmux.bind("t", function()
  gtmux.open{
    width = 16, height = 5,
    component = function(props, ui)
      ui:fill("bg=black")
      ui:box(0, 0, ui.w, ui.h, { style = "fg=green" })
      ui:text(3, 2, gtmux.expand("#{clock}"), "fg=green")
    end,
    on_key = function(key, ui) ui:close() end, -- any key dismisses
  }
end)

-- lock (overrides the server verb): a full-screen cover that hides content and
-- grabs all keys. Unlocks on the lock_password (or any key if none is set).
function gtmux.lock()
  gtmux.open{
    position = "full",
    component = function(props, ui)
      local st = ui:state(); st.buf = st.buf or ""
      ui:fill("bg=black")
      local pw = gtmux.get_option("lock_password")
      local msg = (pw ~= nil and pw ~= "")
        and ("-- locked --  password: " .. string.rep("*", #st.buf))
        or "-- locked --  (press any key)"
      ui:text(2, math.floor(ui.h / 2), msg, "fg=red")
    end,
    on_key = function(key, ui)
      local st = ui:state(); st.buf = st.buf or ""
      local pw = gtmux.get_option("lock_password")
      if pw == nil or pw == "" then ui:close()
      elseif key == "Enter" then if st.buf == pw then ui:close() else st.buf = "" end
      elseif key == "BSpace" then st.buf = st.buf:sub(1, -2)
      elseif #key == 1 then st.buf = st.buf .. key end
    end,
  }
end
-- command-prompt: type a tmux command, Enter runs it (reuses the prompt widget +
-- gtmux.run_command). The richer gtmux.command_prompt() (templates, multi-stage
-- -p prompts) still exists for binds that need %1 substitution.
gtmux.bind(":", function()
  prompt(":", "", function(t) gtmux.run_command(t) end)
end)
gtmux.bind("[", gtmux.enter_copy_mode)
gtmux.bind("]", gtmux.paste)

-- Splits (new panes already open in the active pane's cwd). split_right/split_down
-- name where the new pane lands; split_v/split_h are kept as aliases.
gtmux.bind("|", gtmux.split_right)
gtmux.bind("-", gtmux.split_down)

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
-- gtmux.bind_repeat("m", gtmux.zoom)

-- Custom key tables (tmux bind -T / switch-client -T): key_table() switches the
-- client into a named table for the *next* key only (one-shot), so multi-key
-- sequences work. Example — prefix+g then n/p cycles windows:
-- gtmux.bind("g", function() gtmux.key_table("mygroup") end)
-- gtmux.bind_table("mygroup", "n", gtmux.next_window)
-- gtmux.bind_table("mygroup", "p", gtmux.prev_window)

-- Alert hooks (tmux's alert-bell / alert-activity / alert-silence): gtmux fires
-- the callback when a window's flag rises (edge, not every tick), with a table
-- {event, session, window, name, command, title}. Needs monitor-bell /
-- monitor-activity / monitor-silence on for the window to set the flag.
--
-- Agent awareness: declare which foreground commands are coding agents.
-- gtmux derives a per-pane state from them — "busy" while the pane title
-- carries the busy marker, "done" when the bell rings (turn finished / needs
-- you; focusing the pane clears it), "idle" otherwise. The state shows up
-- three ways: gtmux.on("agent-state") below, the #{pane_agent_state} status
-- format var, and pane:set_border from the hook.
gtmux.agents{
	{ match = "claude", busy = "✳" }, -- Claude Code spins ✳ in the title while working
	{ match = "codex" },
	{ match = "opencode" },
	{ match = "aider" },
	{ match = "gemini" },
	{ match = "amp" },
}
-- Desktop notification when an agent finishes, wherever its window is.
-- (notify-send is Linux; use terminal-notifier on macOS.)
gtmux.on("agent-state", function(p)
	if p.state ~= "done" then return end
	os.execute(string.format(
		"notify-send 'gtmux' '%s: %s is done' 2>/dev/null &",
		p.session, p.command))
end)

-- Command-exit awareness (OSC 133 shell integration): fires when a command run
-- in a pane finishes, with a pane object {session, window, id, exit_code} and a
-- pane:set_border(color) method. The red border clears when you focus the pane.
-- Needs shell integration emitting OSC 133 (starship, or a manual PROMPT_COMMAND).
-- Opt-in — uncomment to flag failed commands:
-- gtmux.on("command-exited", function(p)
--   if p.exit_code ~= 0 then p:set_border("red") end
-- end)

-- Program-aware hook: fires when a pane's foreground program changes (shell →
-- vim, → an agent, etc.), with a pane object {session, window, id, command,
-- from} + pane:set_border(color). Opt-in — uncomment to tint borders per program:
-- gtmux.on("program-changed", function(p)
--   if p.command == "nvim" or p.command == "vim" then p:set_border("blue") end
-- end)

-- Bundled widgets are Lua modules: require("gtmux.<name>") returns a setup
-- function taking an options table (see the module's header for the fields).
-- gtmux.sidebar: left dock with a SESSIONS list and a Clanker (agent panes)
-- section; click a row to switch there. Pair it with a toggle bind:
-- require("gtmux.sidebar"){ size = 25, min_cols = 110 }
-- gtmux.bind("B", function() gtmux.toggle_dock("sidebar") end)
