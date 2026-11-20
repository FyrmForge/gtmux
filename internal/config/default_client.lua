-- gtmux client config: chrome colors and toggles, applied per-attach.
-- Color values are names: black red green yellow blue magenta cyan white,
-- light_grey dark_grey, light_{red,green,yellow,blue,magenta,cyan}.

gtmux.options.mouse = true
gtmux.options.mode_keys = "vi" -- copy-mode keytable: "vi" or "emacs"
-- Line-number gutter in copy-mode: "off", "absolute" (index into the frozen
-- snapshot, oldest scrollback line = 1) or "relative" (distance from the
-- cursor line, which shows 0).
-- gtmux.options.copy_line_numbers = "relative"
-- extended-keys (tmux's option, default off): while a pane app speaks the kitty
-- keyboard protocol, negotiate it with the outer terminal too, so disambiguated
-- keys (Ctrl+I vs Tab, Shift+Enter) reach the app. gtmux does kitty passthrough,
-- not tmux's modifyOtherKeys wire format. Set "on" to enable.
-- gtmux.options.extended_keys = "on"
-- Chars (besides whitespace) that bound copy-mode w/b/e words (tmux's
-- word-separators). Default is all ASCII punctuation, so foo.bar is three words.
-- gtmux.options.word_separators = "!\"#$%&'()*+,-./:;<=>?@[\\]^`{|}~"

-- status-style: fg/bg/attrs as one tmux-style string (comma-separated
-- fg=/bg=/bold/reverse/italics). Partial — only what's listed. The default
-- status bar below is a transparent component and doesn't use it; it colours
-- the plain pickers (choose-window/session), modals and centered notices.
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
-- gtmux.windows()/expand()/context() and paints, transparent (no bar strip —
-- the terminal's own background shows through):
--   left = ● session in the accent colour, then a clickable window list (click
--   selects): the active window bold, the rest dim, a zoomed one marked ⤢ and
--   bell/activity/silence alerts as a red •; right = branch · host · clock, dim;
--   centre = the input mode (gtmux.context().mode): NORMAL dim, anything else
--   (PREFIX, COPY, VISUAL, COMMAND, PICKER, MESSAGE, ...) as a coloured pill,
--   followed by context().mode_info ("line 2/23", "search: foo", a message's
--   text). Copy mode and messages never take the bar over; they live here.
--   Info is cut to fit; the pill is dropped when it would overlap either side.
-- The accent is active_border_fg (green unless your config sets it), so the
-- bar follows the pane-border theme. #{...} still expands via gtmux.expand.
gtmux.widget{ dock = "status", component = function(props, ui)
  local accent = "fg=" .. (gtmux.options.active_border_fg or "green")
  local DIM = "fg=dark_grey"
  -- display width in cells: count UTF-8 lead bytes (# counts bytes, and ●/⤢/·
  -- are 3 bytes each)
  local function len(s) return (select(2, s:gsub("[^\128-\191]", ""))) end
  local x = 1
  local function put(s, style) ui:text(x, 0, s, style); x = x + len(s) end

  put("●", accent)
  x = x + 1
  put(gtmux.expand("#{session}"), accent .. ",bold")
  x = x + 3
  for _, w in ipairs(gtmux.windows()) do
    local mark = ""
    if w.flags:find("Z", 1, true) then mark = " ⤢" end
    local alert = w.flags:find("[!#~]") ~= nil
    local label = w.index .. " " .. w.name .. mark
    local width = len(label) + (alert and 2 or 0)
    ui:child(x, 0, width, 1, function(p, cell)
      cell:text(0, 0, p.idx .. "", p.active and (p.accent .. ",bold") or p.dim)
      cell:text(len(p.idx .. "") + 1, 0, p.rest, p.active and "fg=default,bold" or p.dim)
      if p.alert then cell:text(p.width - 1, 0, "•", "fg=red,bold") end
      cell:on_click(function() gtmux.select_window(p.idx) end)
    end, { idx = w.index, rest = w.name .. mark, active = w.active, alert = alert,
           width = width, accent = accent, dim = DIM })
    x = x + width + 3
  end

  local parts = {}
  local branch = gtmux.expand("#{git_branch}")
  if branch ~= "" then parts[#parts + 1] = "⎇ " .. branch end
  parts[#parts + 1] = gtmux.expand("#{host}")
  parts[#parts + 1] = gtmux.expand("#{clock}")
  local right = table.concat(parts, " · ")
  local ctx = gtmux.context()
  local mode, ctx_info = ctx.mode or "NORMAL", ctx.mode_info
  -- A message needs the room more than branch/host/clock: hide them while up.
  local rx = ui.w - 1
  if mode ~= "MESSAGE" then
    rx = ui.w - len(right) - 1
    ui:text(rx, 0, right, DIM)
  end
  local pill = {
    PREFIX = "yellow", REPEAT = "yellow", COPY = "blue", VISUAL = "magenta",
    ["V-LINE"] = "magenta", ["V-BLOCK"] = "magenta", COMMAND = "cyan",
    PROMPT = "cyan", CONFIRM = "red", MESSAGE = "green",
  }
  local label = " " .. mode .. " "
  local info = ctx_info or ""
  local room = rx - x - 2 -- cells free between the window list and the right side
  if len(label) + 1 + len(info) > room then
    info = info:sub(1, math.max(0, room - len(label) - 2)) -- ASCII-ish cut
    if info ~= "" then info = info .. "…" end
  end
  local total = len(label) + (info ~= "" and 1 + len(info) or 0)
  local mx = math.floor((ui.w - total) / 2)
  mx = math.max(mx, x + 1)
  if mx + total < rx then
    if mode == "NORMAL" then
      ui:text(mx, 0, label, DIM)
    else
      local bg = pill[mode] or (mode:find("^TABLE:") and "yellow") or "light_blue"
      ui:text(mx, 0, label, "fg=black,bold,bg=" .. bg)
    end
    if info ~= "" then ui:text(mx + len(label) + 1, 0, info, "fg=default") end
  end
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
-- gtmux.hide_dock(name) / gtmux.show_dock(name) force one state instead.
-- Every named dock also gets palette commands dock:hide-<name>,
-- dock:show-<name> and dock:toggle-<name> (runnable via gtmux.run_command too).
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
gtmux.bind("x", function() gtmux.confirm_before("kill-pane", "kill this pane?") end)
gtmux.bind("d", gtmux.detach)
gtmux.bind("&", function() gtmux.confirm_before("kill-window", "kill this window?") end)
-- prefix+q kills the session but keeps you in gtmux: detach-on-destroy off on
-- just this session moves the client to another one (it only detaches when
-- this was the last). prefix+Q-style binds without it kill and exit.
gtmux.bind("q", function() gtmux.confirm_before("set-option detach-on-destroy off ; kill-session", "kill this session?") end)
-- rename (prefix+$ / prefix+,): the shared prompt box at the top centre,
-- prefilled with the current name — reads "rename-window: <name>", the same
-- as typing rename-window into the command palette.
gtmux.bind("$", gtmux.rename_session_prompt)
gtmux.bind(",", gtmux.rename_window_prompt)
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
-- Single-line text input with a real cursor, shared by the pickers' filter
-- and name fields. t = { text = "", pos = 0 } (pos = chars before the cursor).
-- input_key edits/moves and returns true when it used the key; the caller
-- checks its own keys (Enter, Tab, C-o, ...) first. input_draw paints
-- label .. text with a reverse-video block on the cursor cell.
local function input_new(text)
  text = text or ""
  return { text = text, pos = #text }
end

local function input_key(t, key)
  local s, p = t.text, t.pos
  if key == "Left" then t.pos = math.max(0, p - 1)
  elseif key == "Right" then t.pos = math.min(#s, p + 1)
  elseif key == "Home" or key == "C-a" then t.pos = 0
  elseif key == "End" or key == "C-e" then t.pos = #s
  elseif key == "BSpace" then
    if p > 0 then t.text, t.pos = s:sub(1, p - 1) .. s:sub(p + 1), p - 1 end
  elseif key == "Delete" then t.text = s:sub(1, p) .. s:sub(p + 2)
  elseif key == "C-u" then t.text, t.pos = s:sub(p + 1), 0
  elseif key == "C-k" then t.text = s:sub(1, p)
  elseif key == "C-w" then
    local head = s:sub(1, p):gsub("%S*%s*$", "")
    t.text, t.pos = head .. s:sub(p + 1), #head
  elseif #key == 1 then t.text, t.pos = s:sub(1, p) .. key .. s:sub(p + 1), p + 1
  elseif key == "Space" then t.text, t.pos = s:sub(1, p) .. " " .. s:sub(p + 1), p + 1
  else return false
  end
  return true
end

local function input_draw(ui, x, y, label, t, style)
  ui:text(x, y, label .. t.text, style)
  local c = t.text:sub(t.pos + 1, t.pos + 1)
  ui:text(x + #label + t.pos, y, c == "" and " " or c, style .. ",reverse")
end

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
    width = 46, height = 16, mode = "PICKER",
    component = function(props, ui)
      local st = ui:state(); st.sel = st.sel or 1; st.input = st.input or input_new()
      st.filter = st.input.text
      ui:fill("bg=black")
      ui:box(0, 0, ui.w, ui.h, { style = "fg=cyan", title = "choose tree" })
      input_draw(ui, 2, 1, "filter: ", st.input, "fg=yellow")
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
      local st = ui:state(); st.sel = st.sel or 1; st.input = st.input or input_new()
      st.filter = st.input.text
      if key == "Down" then st.sel = math.min(#tree_rows(st.filter), st.sel + 1)
      elseif key == "Up" then st.sel = math.max(1, st.sel - 1)
      elseif key == "Enter" then
        local r = tree_rows(st.filter)[st.sel]
        if r then gtmux.switch_session(r.session, r.index) end
        ui:close()
      elseif key == "Escape" then ui:close()
      elseif input_key(st.input, key) then st.filter = st.input.text; st.sel = 1
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
    width = 34, height = 12, mode = "PICKER",
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

-- prefix+s: the fuzzy session finder (gtmux.find_session: type to filter,
-- live pane preview, Enter switches). Set session_switcher = "dock" to drive the
-- sidebar dock instead — same list you already have on screen, with a cursor and
-- a preview of the row you're on. The dock can be hidden (min_cols /
-- toggle_dock), so fall back to the finder rather than let the key go dead.
gtmux.session_switcher = "finder" -- or "dock"
gtmux.bind("s", function()
  -- gtmux.session_dock is set by require("gtmux.sidebar") to whatever name it
  -- was given; read at key time, so load order doesn't matter.
  local dock = gtmux.session_dock or "sidebar"
  if gtmux.session_switcher == "dock" and gtmux.dock_visible(dock) then
    gtmux.focus_dock(dock)
    return
  end
  gtmux.find_session()
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
    width = 16, height = 5, mode = "CLOCK",
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
    position = "full", mode = "LOCKED",
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
-- command palette: fuzzy-find a command (Enter runs it, Tab completes it into
-- the input); a command that takes a session/window/pane/layout/option then
-- lists those to fuzzy-pick. Text matching nothing runs as typed. With -p
-- labels or a template, gtmux.command_prompt() opens the classic prompt.
gtmux.bind(":", gtmux.command_prompt)
-- Your own palette commands: gtmux.command{name, desc, run}. Runs like a bind;
-- name convention is "namespace:verb" (a name equal to a built-in is ignored).
-- gtmux.command{ name = "my:agents", desc = "split a claude pane",
--   run = function() gtmux.run_command("split-window -h claude") end }
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
-- busy is a pane-title substring (or a list of them, for an animated spinner
-- whose frames differ), busy_screen a substring of the pane's bottom
-- rows — for agents that show they are working on screen and never in the
-- title. Neither set means the pane only ever reads idle/done.
gtmux.agents{
	-- Claude Code cycles these four frames at the head of the title while it
	-- works (2.1.228+; braille ⠋⠙⠹… before that). The static ✳ it shows the
	-- rest of the time is NOT a busy marker — matching it inverts the state.
	{ match = "claude", busy = { "◐", "◑", "◒", "◓" } },
	-- codex animates a braille spinner in the title. These four are the UTF-8
	-- lead-byte pairs of the whole braille block (U+2800-U+28FF), so the
	-- substring test matches any frame without listing every glyph.
	{ match = "codex", busy = { "\226\160", "\226\161", "\226\162", "\226\163" } },
	{ match = "opencode", busy_screen = "esc interrupt" }, -- bottom hint line while working
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
-- gtmux.sidebar: left dock listing every session with its agent panes nested
-- beneath; click a row to go there, or focus it and it's the session switcher
-- (j/k, the pane area previews the row, Enter commits). Pair it with a toggle
-- bind, and point prefix+s at it:
-- require("gtmux.sidebar"){ size = 25, min_cols = 110 }
-- gtmux.bind("B", function() gtmux.toggle_dock("sidebar") end)
-- gtmux.session_switcher = "dock"

-- Workspace picker (prefix+P; WORKSPACE_PLAN.md): the projects of a workspace,
-- ⌂ home and open sessions (●) first, then everything A-Z; ↓ marks
-- remote-only repos (Enter clones them). Type to filter (fuzzy: letters in
-- order), Enter opens/switches, Tab cycles workspaces, C-r refreshes from
-- GitHub now, C-o starts a new project (so does Enter on a filter that matches
-- nothing), Esc closes. `gtmux workspace` shows this same picker on a blank
-- screen outside gtmux. Rows re-query the daemon's cache each render, so a
-- background refresh fills in while it's open.
--
-- A workspace with ws.add{ home = "root" | "<folder>" } gets a pinned "⌂ home"
-- row: session <prefix>-home in the workspace folder (or that folder), for an
-- agent that works across every repo (`gtmux ws home [<ws>]`).
--
-- "folders" lists single folders declared with gtmux.workspace.folder{} (notes,
-- dotfiles): one session each, named after the folder; nothing to create.
--
-- "temp" (last in the Tab cycle; `gtmux ws temp`) lists scratch folders under
-- $TMPDIR (`gtmux temp`): Enter on a no-match filter creates a temp with that
-- name, C-o the next numbered one, C-d deletes the selected (y to confirm;
-- refused while its session is open).
--
-- New-project wizard (`gtmux ws new`, or from the picker): name → template
-- (2+ templates) → create on GitHub? (account only) → private? → summary.
-- Esc steps back, C-c cancels. Enter on the summary scaffolds the project and
-- runs the template's commands in its first pane, joined with && (one command
-- per entry: a `;` or `#` inside an entry changes what the chain does).
local function ws_fuzzy(s, pat)
  local i = 1
  s = s:lower()
  for c in pat:lower():gmatch(".") do
    i = s:find(c, i, true)
    if not i then return false end
    i = i + 1
  end
  return true
end

local function ws_wiz_steps(d, wz)
  local steps = { "name" }
  if #d.templates >= 2 then steps[#steps + 1] = "template" end
  if d.account then steps[#steps + 1] = "github" end
  if d.account and wz.gh then steps[#steps + 1] = "private" end
  steps[#steps + 1] = "summary"
  return steps
end

local function ws_wiz_check(d, name)
  if name == "" then return "name required" end
  if not name:match("^[%w_][%w%._%-]*$") then
    return "letters, digits, . _ - only (no leading dot or dash)"
  end
  if #name > 100 then return "max 100 characters" end
  for _, p in ipairs(d.projects) do
    if p.name:lower() == name:lower() then return name .. " already exists" end
  end
end

local function ws_wiz_template(d, wz)
  if #d.templates == 1 then return d.templates[1] end
  return d.templates[wz.tpl] or ""
end

local function ws_wiz_render(inner, d, wz)
  local step = ws_wiz_steps(d, wz)[wz.step]
  local function field(y, label, value, active)
    inner:text(1, y, (active and "> " or "  ") .. label .. value, active and "fg=black,bg=green" or "fg=white")
  end
  local y = 0
  if step == "name" then
    inner:text(1, y, "> ", "fg=black,bg=green")
    input_draw(inner, 3, y, "name:     ", wz.input, "fg=black,bg=green")
  else
    field(y, "name:     ", wz.name, false)
  end
  if #d.templates >= 1 then
    y = y + 1
    field(y, "template: ", ws_wiz_template(d, wz), step == "template")
    if step == "template" then
      for i, n in ipairs(d.templates) do
        y = y + 1
        inner:text(5, y, (i == wz.tpl and "● " or "  ") .. n, i == wz.tpl and "fg=green" or "fg=white")
      end
    end
  end
  if d.account then
    y = y + 1
    field(y, "github:   ", wz.gh and "yes" or "no", step == "github")
    if wz.gh then
      y = y + 1
      field(y, "private:  ", wz.private and "yes" or "no", step == "private")
    end
  end
  local hint = ({
    name = "type a name, Enter next",
    template = "Up/Down choose, Enter next",
    github = "y/n (Space toggles), Enter next",
    private = "y/n (Space toggles), Enter next",
    summary = "Enter create, Esc back",
  })[step]
  inner:text(1, inner.h - 2, hint, "fg=cyan")
  if wz.err then inner:text(1, inner.h - 1, wz.err, "fg=red") end
end

-- ws_wiz_key returns "close" to close the modal, "back" to leave the wizard.
local function ws_wiz_key(key, d, wz)
  local steps = ws_wiz_steps(d, wz)
  local step = steps[wz.step]
  wz.err = nil
  if key == "C-c" then return "close" end
  if key == "Escape" then
    if wz.step == 1 then return "back" end
    wz.step = wz.step - 1
    return
  end
  local enter = key == "Enter" or key == "C-m"
  if step == "name" then
    if enter then
      wz.err = ws_wiz_check(d, wz.name)
      if not wz.err then wz.step = wz.step + 1 end
    elseif input_key(wz.input, key) then
      wz.name = wz.input.text
    end
  elseif step == "template" then
    if key == "Down" or key == "C-n" or key == "j" then
      wz.tpl = math.min(#d.templates, wz.tpl + 1)
    elseif key == "Up" or key == "C-p" or key == "k" then
      wz.tpl = math.max(1, wz.tpl - 1)
    elseif enter then
      wz.step = wz.step + 1
    end
  elseif step == "github" or step == "private" then
    local f = step == "github" and "gh" or "private"
    if key == "y" or key == "n" then
      wz[f] = key == "y"
      wz.step = wz.step + 1
    elseif key == "Space" or key == " " or key == "Left" or key == "Right" or key == "Tab" or key == "C-i" then
      wz[f] = not wz[f]
    elseif enter then
      wz.step = wz.step + 1
    end
  elseif step == "summary" and enter then
    gtmux.workspace.create(d.workspace, wz.name, ws_wiz_template(d, wz), d.account and wz.gh, d.account and wz.gh and wz.private)
    return "close"
  end
end

function gtmux.workspace.picker(ws, opts)
  opts = opts or {}
  local function view(st)
    local d, err = gtmux.workspace.projects(st.ws)
    local rows = {}
    if d then
      st.ws = st.ws or d.workspace -- resolve "this session's workspace" once
      for _, p in ipairs(d.projects) do
        if st.filter == "" or ws_fuzzy(p.name, st.filter) then
          rows[#rows + 1] = p
        end
      end
    end
    return d, rows, err
  end
  local function wizard(name, standalone)
    return { step = 1, name = name or "", input = input_new(name), tpl = 1, gh = false, private = false, standalone = standalone }
  end
  local function init(ui)
    local st = ui:state()
    if st.sel == nil then
      st.sel, st.filter, st.input = 1, "", input_new()
      if ws ~= nil and ws ~= "" then st.ws = ws end
      if opts.new then st.wiz = wizard(opts.name, true) end
    end
    return st
  end
  gtmux.open{
    width = 72, height = 22, mode = "WORKSPACE",
    component = function(props, ui)
      local st = init(ui)
      local d, rows, err = view(st)
      ui:fill("bg=black")
      local inner = ui:box(0, 0, ui.w, ui.h, {
        style = "fg=cyan",
        title = (st.wiz and "new project: " or "workspace: ") .. (d and d.workspace or "?"),
      })
      if not d then
        inner:text(1, 2, err or "no data", "fg=red")
        return
      end
      if st.wiz then
        ws_wiz_render(inner, d, st.wiz)
        return
      end
      input_draw(inner, 1, 0, "filter: ", st.input, "fg=yellow")
      local list_h = inner.h - 3
      if d.status ~= "" then
        inner:text(1, inner.h - 1, d.status, "fg=red")
      end
      local temp = d.workspace == "temp"
      if #rows == 0 and st.filter ~= "" and d.workspace ~= "folders" then
        inner:text(1, 2, "Enter: create " .. (temp and "temp " or "") .. st.filter, "fg=cyan")
      end
      if st.confirm then
        inner:text(1, inner.h - 1, "delete temp " .. st.confirm .. "? y/n", "fg=black,bg=yellow")
      elseif st.err then
        inner:text(1, inner.h - 1, st.err, "fg=red")
      elseif temp then
        inner:text(1, inner.h - 1, "C-o new temp, C-d delete", "fg=cyan")
      end
      if st.sel > #rows then st.sel = math.max(1, #rows) end
      local top = math.max(0, st.sel - list_h)
      for i = 1, math.min(#rows - top, list_h) do
        local p = rows[top + i]
        local mark = p.open and "● " or (p["local"] and "  " or "↓ ")
        local label = mark .. (p.name == "@home" and "⌂ home" or p.name)
        if p.description ~= "" then
          label = label .. "  — " .. p.description
        end
        local sel = (top + i == st.sel)
        local style = "fg=white"
        if sel then
          style = "fg=black,bg=green"
        elseif p.open then
          style = "fg=green"
        end
        inner:text(0, i + 1, (sel and "> " or "  ") .. label, style)
      end
    end,
    on_key = function(key, ui)
      local st = init(ui)
      local d, rows = view(st)
      if st.wiz then
        if not d then
          if key == "Escape" or key == "C-c" then ui:close() end
          return
        end
        local r = ws_wiz_key(key, d, st.wiz)
        if r == "close" or (r == "back" and st.wiz.standalone) then
          ui:close()
        elseif r == "back" then
          st.wiz = nil
        end
        return
      end
      local temp = d and d.workspace == "temp"
      local fixed = d and d.workspace == "folders" -- declared in server.lua: no create
      st.err = nil
      if st.confirm then
        if key == "y" then
          st.err = gtmux.workspace.remove_temp(st.confirm)
        end
        st.confirm = nil
        return
      end
      if key == "Down" or key == "C-n" then
        st.sel = math.max(1, math.min(#rows, st.sel + 1))
      elseif key == "Up" or key == "C-p" then
        st.sel = math.max(1, st.sel - 1)
      elseif key == "Enter" or key == "C-m" then
        local p = rows[st.sel]
        if not p and st.filter ~= "" and temp then
          gtmux.workspace.open("temp", st.filter)
          ui:close()
          return
        end
        if not p and st.filter ~= "" and not fixed then
          st.wiz = wizard(st.filter, false)
          return
        end
        if not p and fixed then return end
        if p and d then gtmux.workspace.open(d.workspace, p.name) end
        ui:close()
      elseif key == "C-o" and temp then
        gtmux.workspace.open("temp", "")
        ui:close()
      elseif key == "C-o" and not fixed then
        st.wiz = wizard(st.filter, false)
      elseif key == "C-d" and temp then
        local p = rows[st.sel]
        if p then st.confirm = p.name end
      elseif key == "Escape" or key == "C-c" then
        ui:close()
      elseif key == "Tab" or key == "C-i" then
        if d and #d.workspaces > 1 then
          for i, n in ipairs(d.workspaces) do
            if n == d.workspace then
              st.ws = d.workspaces[i % #d.workspaces + 1]
              break
            end
          end
          st.sel, st.filter, st.input = 1, "", input_new()
        end
      elseif key == "C-r" then
        gtmux.workspace.refresh(d and d.workspace or st.ws)
      elseif input_key(st.input, key) then
        st.filter, st.sel = st.input.text, 1
      end
    end,
  }
end
gtmux.bind("P", function() gtmux.workspace.picker() end)
