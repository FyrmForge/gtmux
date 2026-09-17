-- gtmux.sidebar: a bordered left dock holding one list — every session, with
-- the agent panes running inside it (claude/codex/opencode…) nested beneath,
-- coloured by whether the agent is working or awaiting your input.
--
-- Click any row to go there — a session row or one of its agent rows. Or focus
-- the dock (gtmux.focus_dock(name), or pane-navigate into it) and it becomes
-- the session switcher: j/k walks the SESSION rows (agents are context, not
-- stops), the pane area previews the one you're on WITHOUT switching, Enter
-- commits, Esc backs out. The preview is a snapshot per row, not a live feed.
-- Unfocused, the cursor sits on the session you're attached to.
--
-- Usage (all fields optional, defaults shown):
--   require("gtmux.sidebar"){
--     dock = "left", size = 25, name = "sidebar", interval = 1,
--     min_cols = 110,   -- auto-hide on narrower clients; gtmux.toggle_dock(name) forces it
--     spinner = true,   -- animate a spinner glyph on working agents
--     title = true,     -- show the agent's current task title
--     preview = true,   -- preview the cursor's row in the pane area while focused
--     focus = "both",   -- "nav" | "bind" | "both" | "" (not focusable)
--     -- Agent row template; "\n" starts a new line, indented under its session.
--     -- Fields: {tag} {glyph} {name} {session} {window} {pane} {title} {command}.
--     fmt = "{glyph} {name}\n{title}",
--     agents = { claude = "cl", codex = "cx", opencode = "oc",
--                aider = "ai", gemini = "gm", amp = "am" }, -- command -> tag
--     names  = { claude = "Claude", codex = "Codex", opencode = "OpenCode",
--                aider = "Aider", gemini = "Gemini", amp = "Amp" }, -- command -> display name
--   }
local defaults = {
  dock = "left", size = 25, name = "sidebar", interval = 1, min_cols = 110,
  spinner = true, title = true, preview = true, focus = "both",
  fmt = "{glyph} {name}\n{title}",
  agents = { claude = "cl", codex = "cx", opencode = "oc",
             aider = "ai", gemini = "gm", amp = "am" },
  names = { claude = "Claude", codex = "Codex", opencode = "OpenCode",
            aider = "Aider", gemini = "Gemini", amp = "Amp" },
}

return function(opts)
  opts = setmetatable(opts or {}, { __index = defaults })

  -- Tell the prefix+s bind which dock to drive, so a renamed sidebar still
  -- works as the session switcher instead of silently falling back.
  gtmux.session_dock = opts.name

  -- Per-pane "you've seen it" state lives on the SERVER as a global user option
  -- (@seen_<paneid> = "1"), so every attached client, in any session, agrees:
  -- an idle agent focused from anywhere goes grey everywhere. Set/unset via
  -- run_command from the draw (draws may emit ops); read back from the snapshot
  -- with gtmux.global_option. Re-armed (unset) whenever the agent works again.
  -- The local table is kept alongside for two real reasons: it masks the
  -- run_command -> next-snapshot round-trip (a focused idle pane greys
  -- instantly instead of one tick late), and it carries the behaviour on an
  -- old server whose snapshots don't include global options.
  local localSeen = {} -- tri-state override: nil = trust the snapshot
  local function seen(id)
    local g = gtmux.global_option("@seen_" .. id) ~= ""
    local o = localSeen[id]
    if o == nil then return g end
    if o == g then localSeen[id] = nil; return g end -- snapshot caught up
    return o
  end
  local function setSeen(id, v)
    if seen(id) == v then return end -- also stops re-queueing the same command every draw
    localSeen[id] = v
    gtmux.run_command((v and "set -g @seen_" or "set -g -u @seen_") .. id .. (v and " 1" or ""))
  end
  -- Agent panes present last draw: when one vanishes (pane closed), its
  -- @seen_ option is unset, or dead entries ride in every snapshot forever.
  local lastAgents = {}
  local clankerFrame = 0
  local clankerSpin = { "⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏" }

  -- items is what the keyboard cursor walks: the SESSION rows only, in display
  -- order, rebuilt every draw. Agent rows render nested beneath their session
  -- but aren't stops — you switch to a session, and the agents under it are the
  -- reason you're picking it. They stay clickable. Each entry: { target, row }
  -- — where selecting it goes and its index into the rendered row list.
  -- Indexing items (not canvas rows) also means the cursor can't land on a
  -- blank spacer.
  local items = {}
  local sel = 1        -- fallback cursor position, if selName is gone
  -- The cursor remembers the session NAME, not the index: the list is rebuilt
  -- every draw, and killing a session ABOVE the cursor shifts an index onto a
  -- different session — Enter would then switch you somewhere you never saw.
  local selName = nil
  local scrollTop = 1  -- first rendered row shown, so the cursor can't walk off
  local rowTarget = {} -- canvas row -> switch target, for on_click (sessions AND agents)

  -- glyphFor classifies one agent pane: the glyph + style that say whether it's
  -- working, blocked on you, or done. State comes from the client's agent
  -- classifier (gtmux.agents{} -> find_panes row .state): "busy" while the busy
  -- marker shows, "done"/"idle" when the agent stopped for you, "" = no
  -- classifier matched. One classifier for the sidebar, #{pane_agent_state} and
  -- gtmux.on("agent-state") -- tune gtmux.agents{} to affect them all.
  local function glyphFor(p, focusedPane)
    if p.title:find("Action Required", 1, true) then    -- codex: permission prompt
      setSeen(p.id, false)
      return "⚠", "fg=yellow,bold"                      -- blocked on you
    elseif p.state == "busy" then
      setSeen(p.id, false)                              -- re-arm for next idle
      return (opts.spinner and clankerSpin[(clankerFrame % #clankerSpin) + 1] or "~"), "fg=blue"
    elseif p.state == nil or p.state == "" then
      return "?", "fg=magenta"                          -- state unknown
    end
    if focusedPane == ("%" .. p.id) then setSeen(p.id, true) end
    if seen(p.id) or focusedPane == ("%" .. p.id) then
      return "·", "fg=dark_grey"                        -- seen it -> dismissed
    end
    return "!", "fg=red,bold"                           -- awaiting you
  end

  gtmux.widget{ dock = opts.dock, size = opts.size, fg = "white", bg = "", interval = opts.interval,
    name = opts.name, min_cols = opts.min_cols, -- auto-hide on narrow clients; toggle_dock(name) forces it
    focus = opts.focus,
    component = function(props, ui)
      local st = ui:state()
      -- box returns its interior as a clipped child: text drawn through `inner`
      -- is truncated at the border instead of overwriting it. The title stays on
      -- `ui` on purpose — it sits ON the top border row.
      -- Focused, the whole frame goes green: the cursor alone is one cell, easy
      -- to miss on a wide screen, and keys are being swallowed by this dock.
      local edge = st.focused and "green" or "cyan"
      local inner = ui:box(0, 0, ui.w, ui.h, "fg=" .. edge .. ",rounded")
      if not inner then return end -- canvas under 2 rows: no interior to draw in
      ui:text(2, 0, " gtmux ", "fg=" .. edge .. ",bold")
      inner:text(1, 0, "SESSIONS", "fg=cyan,bold")

      local ctx = gtmux.context()
      local cur, focusedPane = ctx.session, ctx.pane
      clankerFrame = clankerFrame + 1

      -- One pass over every agent pane, bucketed by session, so the nested
      -- render below is a lookup instead of a scan per session.
      local bySession, nowAgents = {}, {}
      for _, p in ipairs(gtmux.find_panes({})) do
        if opts.agents[p.command] then
          nowAgents[p.id] = true
          bySession[p.session] = bySession[p.session] or {}
          table.insert(bySession[p.session], p)
        end
      end

      -- Render into a row list first, then paint a window of it: the list can
      -- be taller than the dock, and painting straight to the canvas would clip
      -- the cursor out of sight instead of scrolling to it.
      items, rowTarget = {}, {}
      local rows = {}   -- { col, text, style, target } in list order; text "" = spacer
      local function add(col, text, style, target)
        rows[#rows + 1] = { col = col, text = text, style = style, target = target }
        return #rows
      end

      local firstSession, prevWasCur = true, false
      for _, s in ipairs(gtmux.sessions()) do
        -- A dim rule between groups, not a blank line: at a glance you can see
        -- where one session's agents end and the next session begins. Inset a
        -- column each side so it reads as a divider, not a second border.
        if not firstSession then
          -- The two rules bracketing the attached session light up, so its whole
          -- block reads as one lit band even when the name scrolls out of view.
          local lit = (s.name == cur) or prevWasCur
          add(1, string.rep("─", math.max(0, inner.w - 2)), lit and "fg=green" or "fg=dark_grey")
        end
        firstSession = false
        prevWasCur = (s.name == cur)
        -- The current session is colour only: "> " is the cursor now.
        local r = add(3, s.name, (s.name == cur) and "fg=green,bold" or "fg=white", s.name)
        items[#items + 1] = { target = s.name, row = r }
        local firstAgent = true
        for _, p in ipairs(bySession[s.name] or {}) do
          if not firstAgent then add(1, "") end -- blank between agents, not after the last
          firstAgent = false
          local glyph, style = glyphFor(p, focusedPane)
          local disp = ""
          if opts.title and glyph ~= "?" then
            -- strip one leading status glyph (U+2000-2FFF: spinners) + space;
            -- the old %w scan ate everything before the first ASCII letter
            disp = (p.title:gsub("^\226[\128-\191][\128-\191]%s*", "", 1))
          end
          local f = { tag = opts.agents[p.command], glyph = glyph, name = opts.names[p.command] or p.command,
                      session = p.session, window = tostring(p.window), pane = tostring(p.number),
                      title = disp, command = p.command }
          local first = true
          -- Two steps on purpose: gsub's second return value must be dropped
          -- into a local before the result is chained into gmatch.
          local text = opts.fmt:gsub("{(%w+)}", function(k) return f[k] or "" end)
          for line in (text .. "\n"):gmatch("(.-)\n") do
            if line:match("%S") then                    -- skip a line that emptied out
              add(5, line, style, first and (p.session .. ":%" .. p.id) or nil)
              first = false
            end
          end
        end
      end

      -- Unfocused, the cursor isn't a leftover selection — it's the "you are
      -- here" marker, so it tracks the attached session. Focused, it's yours.
      if not st.focused then selName = cur end
      sel = math.min(math.max(sel, 1), math.max(#items, 1))
      for i, it in ipairs(items) do
        if it.target == selName then sel = i end
      end
      if items[sel] then selName = items[sel].target end

      -- Scroll just enough to keep the cursor's row on screen. List area is
      -- every inner row below the SESSIONS header.
      local avail = math.max(1, inner.h - 1)
      local selRow = items[sel] and items[sel].row or 1
      if selRow < scrollTop then scrollTop = selRow end
      if selRow > scrollTop + avail - 1 then scrollTop = selRow - avail + 1 end
      scrollTop = math.min(math.max(scrollTop, 1), math.max(1, #rows - avail + 1))

      for i = scrollTop, math.min(#rows, scrollTop + avail - 1) do
        local r, ry = rows[i], 1 + i - scrollTop -- ry: inner row
        if r.text ~= "" then inner:text(r.col, ry, r.text, r.style) end
        if r.target then rowTarget[ry + 1] = r.target end -- on_click gets PARENT rows
      end
      if items[sel] and selRow >= scrollTop and selRow <= scrollTop + avail - 1 then
        inner:text(1, 1 + selRow - scrollTop, ">", st.focused and "fg=green,bold" or "fg=dark_grey")
      end

      for id in pairs(lastAgents) do
        if not nowAgents[id] then
          setSeen(id, false)
          localSeen[id] = nil -- pane is gone; drop the override too
        end
      end
      lastAgents = nowAgents
    end,
    on_key = function(key, ui)
      if #items == 0 then
        if key == "Escape" or key == "q" then ui:close() end
        return
      end
      if key == "Down" or key == "j" then sel = math.min(#items, sel + 1); selName = items[sel].target
      elseif key == "Up" or key == "k" then sel = math.max(1, sel - 1); selName = items[sel].target
      elseif key == "Enter" then
        gtmux.switch_client("-t", items[sel].target)
        ui:close()                                      -- also clears the preview
        return
      elseif key == "Escape" or key == "q" then ui:close(); return
      else return                                       -- unhandled key: don't re-preview
      end
      -- Moving the cursor shows where you'd land, without going there. The
      -- client drops a snapshot that arrives after the cursor moved on.
      if opts.preview then gtmux.preview(items[sel].target) end
    end,
    on_click = function(hit)
      local target = rowTarget[hit.line]
      if target then gtmux.switch_client("-t", target) end
    end }
end
