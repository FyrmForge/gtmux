-- gtmux.sidebar: a bordered left dock with a live SESSIONS list (current
-- highlighted, click to switch) and a Clanker section (every pane running an
-- agent — claude/codex/opencode — across all sessions), coloured by whether
-- the agent is working or awaiting your input. Click a session row to switch
-- to it; click an agent row to land on that pane.
--
-- Usage (all fields optional, defaults shown):
--   require("gtmux.sidebar"){
--     dock = "left", size = 25, name = "sidebar", interval = 1,
--     min_cols = 110,   -- auto-hide on narrower clients; gtmux.toggle_dock(name) forces it
--     spinner = true,   -- animate a spinner glyph on working agents
--     title = true,     -- show the agent's current task title
--     -- Row template; "\n" starts a new line. Fields: {tag} {glyph} {session}
--     -- {window} {pane} {title} {command}. {title} is dropped when unknown.
--     fmt = "{tag}{glyph}{session}:{window}.{pane}\n  {title}",
--     agents = { claude = "cl", codex = "cx", opencode = "oc",
--                aider = "ai", gemini = "gm", amp = "am" }, -- command -> tag
--   }
local defaults = {
  dock = "left", size = 25, name = "sidebar", interval = 1, min_cols = 110,
  spinner = true, title = true,
  fmt = "{tag}{glyph}{session}:{window}.{pane}\n  {title}",
  agents = { claude = "cl", codex = "cx", opencode = "oc",
             aider = "ai", gemini = "gm", amp = "am" },
}

return function(opts)
  opts = setmetatable(opts or {}, { __index = defaults })

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
  -- line -> switch-client target for on_click: a session row maps to its name,
  -- an agent row to "session:%paneid" (lands on the pane). Recorded at draw time
  -- rather than matched from the row text, which is truncated for long names.
  local lineSession = {}
  local clankerSpin = { "⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏" }
  gtmux.widget{ dock = opts.dock, size = opts.size, fg = "white", bg = "", interval = opts.interval,
    name = opts.name, min_cols = opts.min_cols, -- auto-hide on narrow clients; toggle_dock(name) forces it
    draw = function(c)
      -- box returns its interior as a clipped child: text drawn through `inner`
      -- is truncated at the border instead of overwriting it. The title and the
      -- hline stay on `c` on purpose — the title sits ON the top border row, and
      -- hline spans the full width so it merges into the sides as tees.
      local inner = c:box(0, 0, c.w, c.h, "fg=cyan,rounded")
      if not inner then return end -- canvas under 2 rows: no interior to draw in
      c:text(2, 0, " gtmux ", "fg=cyan,bold")

      inner:text(1, 0, "SESSIONS", "fg=cyan,bold")
      local ctx = gtmux.context()
      local cur, y = ctx.session, 2               -- y stays in PARENT coords
      lineSession = {}
      for _, s in ipairs(gtmux.sessions()) do
        local here = (s.name == cur)
        inner:text(1, y - 1, (here and "> " or "  ") .. s.name,
               here and "fg=green,bold" or "fg=white")
        if y <= c.h - 2 then lineSession[y] = s.name end
        y = y + 1
      end

      c:hline(y, "fg=dark_grey"); y = y + 1
      inner:text(1, y - 1, "Clanker", "fg=magenta,bold"); y = y + 1
      -- State comes from the client's agent classifier (gtmux.agents{} ->
      -- find_panes row .state): "busy" while the busy marker shows, "done"/
      -- "idle" when the agent stopped for you, "" = no classifier matched
      -- (state unknown, ?). One classifier for the sidebar, #{pane_agent_state}
      -- and gtmux.on("agent-state") -- tune gtmux.agents{} to affect them all.
      clankerFrame = clankerFrame + 1
      local focused = ctx.pane
      local tags = opts.agents
      local shown = 0
      local nowAgents = {}
      for _, p in ipairs(gtmux.find_panes({})) do
        local tag = tags[p.command]
        if tag then
          nowAgents[p.id] = true
          local glyph, style
        if p.title:find("Action Required", 1, true) then    -- codex: permission prompt
          setSeen(p.id, false)
          glyph, style = "⚠", "fg=yellow,bold"                 -- blocked on you
        elseif p.state == "busy" then
          setSeen(p.id, false)                                -- re-arm for next idle
          glyph = opts.spinner and clankerSpin[(clankerFrame % #clankerSpin) + 1] or "~"
          style = "fg=blue"
        elseif p.state == nil or p.state == "" then
          glyph, style = "?", "fg=magenta"                    -- state unknown
        else                                                  -- idle/done: stopped for you
          if focused == ("%" .. p.id) then setSeen(p.id, true) end
          if seen(p.id) or focused == ("%" .. p.id) then
            glyph, style = "·", "fg=dark_grey"                -- seen it -> dismissed
          else
            glyph, style = "!", "fg=red,bold"                 -- awaiting you
          end
        end
        local disp = ""
          if opts.title and glyph ~= "?" then
            -- strip one leading status glyph (U+2000-2FFF: spinners) + space;
          -- the old %w scan ate everything before the first ASCII letter
          disp = (p.title:gsub("^\226[\128-\191][\128-\191]%s*", "", 1))
          end
          local f = { tag = tag, glyph = glyph, session = p.session, window = tostring(p.window),
                      pane = tostring(p.number), title = disp, command = p.command }
          local rows = opts.fmt:gsub("{(%w+)}", function(k) return f[k] or "" end)
          for line in (rows .. "\n"):gmatch("(.-)\n") do
            if line:match("%S") then                            -- skip a line that emptied out
              inner:text(1, y - 1, line, style)
              if y <= c.h - 2 then lineSession[y] = p.session .. ":%" .. p.id end
              y = y + 1
            end
          end
          y = y + 1                                             -- blank line between agents
          shown = shown + 1
        end
      end
      if shown == 0 then inner:text(1, y - 1, "(none)", "fg=dark_grey") end
      for id in pairs(lastAgents) do
        if not nowAgents[id] then
          setSeen(id, false)
          localSeen[id] = nil -- pane is gone; drop the override too
        end
      end
      lastAgents = nowAgents
    end,
    on_click = function(hit)
      local ls = lineSession[hit.line]
      if ls then gtmux.switch_client("-t", ls) end
    end }
end
