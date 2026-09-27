---
description: Set up Purser roles and optional project trust and profile.
allowed-tools: Bash(python *)
---

1. Run `python "${CLAUDE_PLUGIN_ROOT}/.claude/hooks/orchestrator/purser_init.py" --detect --json` in the current project. Show which agents are installed and the current roles, trust, and profile state.
2. Tell the user in one sentence, in bold, that Purser learns as it works in this project: it measures how long its work really takes, sharpens its estimates, and points out where tokens and time can be saved. Then tell the user that Claude Code is the orchestrator. Ask in the user's language: "Who writes the code? 1) Codex 2) Claude". Add "3) OpenCode (free models included)" only when detection reports OpenCode available, and "Antigravity (agy)" only when detection reports agy available. Show which agents are installed. If Codex is chosen, ask how Purser should work using this approved source text, rendered in the user's language without changing the three options or their meaning: "Purser nasıl çalışsın? 1) Dengeli (önerilen): Her iş için uygun modeli Purser seçer; kullandıkça öğrenir, zamandan ve kotadan tasarruf eder. 2) Maksimum zeka: Her işte en güçlü model ve en yüksek düşünme seviyesi; daha yavaş, kotayı daha hızlı tüketir. 3) Kendim seçeceğim: Kodlayıcının modelini ve düşünme seviyesini sen belirlersin; Purser dokunmaz." Pass the selected option as `--mode balanced`, `--mode maximum`, or `--mode manual` to both `--plan` and `--apply`. For manual mode, run `purser_init.py --list-models --json --coder codex`, show only its listed models, and let the user choose one. Show the chosen model's listed efforts and let the user choose one; pass the choice with `--model MODEL` and `--effort EFFORT` to both commands. Do not accept a model or effort outside the listing. If OpenCode or agy is chosen, ask "Which model? (pick from the list)" and show that agent's detected model list. Pass the selected model as `--model MODEL` to `--plan` and `--apply`, without `--mode`. The agy list comes from `agy models` and contains only Gemini models because Reviewer-1 also runs agy. Antigravity routes use medium effort by default. If Claude is chosen, ask no mode or model question.
3. Ask whether another agent is available. If `agy` is detected, ask whether to use it as the independent reviewer. Otherwise choose `reviewer = "none"`. Explain: without an independent reviewer, every delegated call gets FULL review.
4. Ask whether to trust the current project and whether to scaffold `.orchestrator/profile.toml`.
5. If the repo has a UI framework in `package.json`, `.tsx`, `.vue`, `.svelte`, or `.html` files, or is empty, and neither `AGENTS.md` nor `.orchestrator` records a Ponytail decision, offer once in the user's language: "⚓ **Building UI or web screens? Purser can switch on Ponytail: shortest working code, no needless abstractions, noticeably fewer tokens on UI work.** Turn it on for this project?" On yes, copy `${CLAUDE_PLUGIN_ROOT}/addons/ponytail/ponytail.md` to `.orchestrator/ponytail.md` and add this exact line to `AGENTS.md` (create it if missing):

   > Before writing code, read `.orchestrator/ponytail.md`. The repository contract and the task's acceptance criteria outrank it.

   On no, do nothing.
6. Run `python "${CLAUDE_PLUGIN_ROOT}/.claude/hooks/orchestrator/purser_init.py" --plan --coder X --reviewer Y`, adding the selected `--mode MODE` and manual `--model MODEL --effort EFFORT` only when Codex was chosen, or `--model MODEL` only when OpenCode or agy was chosen. Add `--trust` and `--scaffold-profile` only if chosen. Show the entire plan, including its resolved route-to-model/effort pairs. Ask for explicit yes to this exact plan. Only after that yes, run the same options with `--apply` instead of `--plan`.
7. Run `/purser:check` and show its result.
8. Offer `/purser:optimize` in one sentence; do not run it automatically.

The plan shows the Python 3.11+ interpreter path that `--apply` stores in `~/.orchestrator/python` for plugin launches.

Never edit `~/.orchestrator/config.toml` or `.orchestrator/profile.toml` by hand. If detection or planning fails, show the error and stop setup.
