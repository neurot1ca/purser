---
description: Set up Purser roles and optional project trust and profile.
allowed-tools: Bash(*), AskUserQuestion
---

Flow rules:
- Every question is one `AskUserQuestion` picker. Put the recommended option first and mark it “(recommended)” in the user's language. For dynamic model and effort listings, follow their recommendation metadata and mark nothing when none is specified. Keep fixed recommendations for the other pickers. Never ask a numbered text question.
- Speak the user's language from the first line, including in projects without Purser.
- Start every message with ⚓ and use a receipt: a bold headline and short italic detail.
- English is the master. Use the approved Turkish renderings below only when the user's language is Turkish; keep every question and option in one run in that language. Translate English into other languages without changing its meaning.

Resolve and run the bundled Purser binary as in `commands/optimize.md`, using the `init` subcommand. Pass the existing arguments unchanged:

```bash
os="$(uname -s)"
arch="$(uname -m)"
case "$os" in MINGW*|MSYS*|CYGWIN*) os=windows ;; Darwin) os=darwin ;; Linux) os=linux ;; *) echo "Unsupported OS: $os" >&2; exit 1 ;; esac
case "$arch" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; amd64|arm64) ;; *) echo "Unsupported architecture: $arch" >&2; exit 1 ;; esac
suffix=""; [ "$os" = windows ] && suffix=".exe"
"${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-${os}-${arch}${suffix}" init --detect --json
```

1. Show installed agents and the current roles, trust, and profile state from `init --detect --json` in one ⚓ receipt. Read `maintenance_enabled`: `null` means no project choice has been saved; `true` or `false` means the user has already chosen, so do not offer maintenance again.
2. In the user's language, show this intro as one ⚓ receipt, then say Claude Code is the orchestrator. Ask who writes the code in one `AskUserQuestion` picker. Put Codex first as “(recommended)”; add OpenCode only when detection reports it available, labelled exactly **Free OpenCode models**, and add Antigravity (agy) only when detection reports agy available. Show installed agents.

   EN: ⚓ **Purser learns as it works here: it measures how long jobs really take, sharpens its estimates and shows where tokens and time can be saved.** *It runs independent jobs side by side, each in its own worktree; a job that touches files another job is changing waits its turn. When a task needs a plan, Purser proposes one first, then works through it step by step.*

   TR: ⚓ **Purser bu projede çalıştıkça öğrenir: işlerin gerçekte ne kadar sürdüğünü ölçer, tahminlerini keskinleştirir ve token ile zamandan nerede tasarruf edilebileceğini gösterir.** *Birbirinden bağımsız işleri yan yana, her birini kendi çalışma ağacında yürütür; başka bir işin değiştirdiği dosyalara dokunacak iş sırasını bekler. Bir görev plan gerektiriyorsa Purser önce plan önerir, sonra adım adım uygular.*

   If Codex is chosen, ask in one `AskUserQuestion` picker: EN **How should Purser work?** / TR **Purser nasıl çalışsın?** Put Balanced first and mark it “(recommended)” in the user's language. Keep the same `--mode balanced|maximum|manual` choices and pass the selected flag to both `--plan` and `--apply`:

   - EN **Balanced (recommended for the biggest savings):** *light tasks go to a fast model, hard ones to a stronger one; if a light attempt fails, Purser retries on the stronger model by itself. (You can switch to the stronger, faster model anytime with `/purser:gear-up`, and back with `/purser:gear-down`.)*
   - TR **Dengeli (yüksek tasarruf için önerilir):** *hafif işler hızlı modele, zor işler güçlü modele gider; hafif deneme başarısız olursa Purser kendiliğinden güçlü modelle yeniden dener. (`/purser:gear-up` ve `/purser:gear-down` komutlarıyla istediğin zaman daha güçlü ve hızlı modele geçip geri dönebilirsin.)*
   - EN **Maximum intelligence:** *the strongest model at the highest thinking level on every task; slower and uses quota faster.*
   - TR **Maksimum zeka:** *her işte en güçlü model, en yüksek düşünme seviyesi; daha yavaş, kotayı daha hızlı harcar.*
   - EN **I'll choose:** *you pick the coder's model and thinking level; Purser leaves them alone.*
   - TR **Kendim seçeceğim:** *kodlayıcının modelini ve düşünme seviyesini sen seçersin; Purser dokunmaz.*

   For manual mode, run `init --list-models --json --coder codex`, show only its listed models, and let the user choose one in an `AskUserQuestion` picker. Mark “(recommended)” only on a model whose listing has `recommended: true`; mark no model when none is recommended. Show the chosen model's listed efforts and let the user choose one in a picker. Mark “(recommended)” only on the effort named by that model's `recommended_effort`; mark no effort when it is null or absent. Pass the choice with `--model MODEL` and `--effort EFFORT` to both commands. Do not accept a model or effort outside the listing. If OpenCode or agy is chosen, show its detected options by model name in a picker and mark “(recommended)” only on options whose detection has `recommended: true`; mark none when none is recommended. Group all `opencode/*-free` models and `opencode/big-pickle` under **Free OpenCode models**; show each paid/provider model (including DeepSeek models) as its own option. For an OpenCode coder route, pass the exact selected model as `--model MODEL` to both commands, without `--mode`. The agy options come from `agy models` and contain only Gemini models because Reviewer-1 also runs agy. Antigravity routes use medium effort by default. If Claude is chosen, ask no mode or model question and say in one ⚓ receipt, translated to the user's language: “Claude writes the code and the plans. Purser picks a tier for each task and always runs its newest model: Haiku for simple and operational work, Sonnet for ordinary work, Opus for important work and plans.”
3. After the coder is chosen, rerun `init --detect --json --coder CODER`, adding `--model MODEL` when a specific coder model was selected. For Codex, OpenCode, or agy, ask in one `AskUserQuestion` picker which coder to use if the primary coder's quota runs out or it fails. Use the existing Turkish wording verbatim for Turkish users: “Ana kodlayıcının kotası dolar ya da çalışmazsa hangi kodlayıcıya geçilsin? Geçmeden önce her seferinde sana sorulur.” Translate it for other languages. Show only working coders from detection, by model name, plus “No fallback”; put “No fallback” first and mark it “(recommended)” in the user's language. Never offer a fallback for Claude as primary because its dispatch boundary is not established. On a selected fallback, pass `--fallback-agent AGENT --fallback-model MODEL` to both plan and apply; for “No fallback”, pass neither flag.

   Read `reviewer_options` from detection JSON. Ask in one `AskUserQuestion` picker:

   EN: ⚓ **Who should review the coder's work (Reviewer-1)?** *We recommend a reviewer independent of the coder and Claude, but any installed agent works, or you can turn it off. You can switch later with `/purser:reviewer`.*

   TR: ⚓ **Kodlayıcının işini kim incelesin (Reviewer-1)?** *Kodlayıcıdan ve Claude'dan bağımsız bir inceleyici öneriyoruz, ama kurulu herhangi bir agent olabilir ya da kapatabilirsin. Sonra `/purser:reviewer` ile değiştirebilirsin.*

   Show detected `reviewer_options` by model name, plus Off. Put an independent reviewer first as “(recommended)” in the user's language. Do not mark same-family options. Off option:

   - EN **Off:** *no independent review; Claude reviews alone.*
   - TR **Kapalı:** *bağımsız inceleme yok; Claude tek başına inceler.*

   For a selected option, pass `--reviewer AGENT --reviewer-model MODEL` to both plan and apply; use `--reviewer none` for Off. Keep the `--reviewer` and `--reviewer-model` flags.
4. Only when OpenCode is detected, ask the following in one `AskUserQuestion` picker using “Turn it on” as the recommended yes option. Keep the `--opencode-mechanical` / `--no-opencode-mechanical` flags in both plan and apply.

   EN: ⚓ **Free models for small chores** *Counting, status checks and summaries can run on OpenCode's free models, so they cost no quota. Each chore goes to the free model with the best record in your projects; if that model fails or is unavailable, the next one takes over. Purser scores every result (passed review and tests or not) and ranks the models by their last 20 results. When a new free model appears, Purser races it against the leader: now and then it reruns a finished chore on the new model in the background, and after five results the newcomer joins the ranking. Your work is never touched. Turn it on?*

   TR: ⚓ **Küçük işler için ücretsiz modeller** *Sayım, durum kontrolü ve özet gibi işler OpenCode'un ücretsiz modelleriyle çalışabilir, kota harcamaz. Her iş, projelerinde en iyi sonucu vermiş ücretsiz modele gider; o model başarısız olur ya da yanıt vermezse sıradaki devralır. Purser her sonucu puanlar (inceleme ve testlerden geçti mi), modelleri son 20 sonuçlarına göre sıralar. Yeni bir ücretsiz model gelince Purser onu öndeki modelle yarıştırır: ara sıra bitmiş bir işi arka planda yeni modelde tekrar çalıştırır, beş sonuçtan sonra yeni model sıralamaya girer. Senin işine hiç dokunulmaz. Açayım mı?*
5. Ask whether to trust the current project and whether to scaffold `.orchestrator/profile.toml`, each in its own `AskUserQuestion` picker. Put “No” first and mark it “(recommended)” in the user's language. Pass `--trust` and `--scaffold-profile` only if chosen.
6. If the repo has a UI framework in `package.json`, `.tsx`, `.vue`, `.svelte`, or `.html` files, or is empty, and neither `AGENTS.md` nor `.orchestrator` records a Ponytail decision, offer once in the user's language in one ⚓ receipt and `AskUserQuestion` picker: “Building UI or web screens? Purser can switch on Ponytail: shortest working code, no needless abstractions, noticeably fewer tokens on UI work. Turn it on for this project?” Put “No” first and mark it “(recommended)” in the user's language. On yes, copy `${CLAUDE_PLUGIN_ROOT}/addons/ponytail/ponytail.md` to `.orchestrator/ponytail.md` and add this exact line to `AGENTS.md` (create it if missing):

   > Before writing code, read `.orchestrator/ponytail.md`. The repository contract and the task's acceptance criteria outrank it.

   On no, do nothing.
7. When `maintenance_enabled` is `null`, for every chosen coder, after the existing setup questions and before showing the plan, ask once in the user's language with one `AskUserQuestion` picker. Use “Turn it on” as the recommended yes option and keep `--maintenance` / `--no-maintenance` in both plan and apply. For Codex, agy, Claude, and OpenCode free models, replace `<coder>` with the chosen coder model name and use the subscription/free-model receipt. For a paid OpenCode provider model, replace `<coder>` with the chosen model name and use the API-credit receipt.

   EN: ⚓ **Purser can keep this project tidy in the background.** *When you are idle it runs read-only scans with your coder (<coder>) on its subscription, never a paid API and never during a job:*
   - *cleanup candidates (dead code, stale files)*
   - *missing tests and the project's test coverage*
   - *a short report; nothing is changed*
   *Turn it on? You can change it anytime with `/purser:optimize`.*

   TR: ⚓ **Purser bu projeyi arka planda düzenli tutabilir.** *Sen boştayken kodlayıcınla (<coder>) aboneliği üzerinden yalnızca okuma yapan taramalar çalıştırır; ücretli API kullanmaz, iş çalışırken devreye girmez:*
   - *temizlenebilecekler (ölü kod, eskimiş dosyalar)*
   - *eksik testler ve proje test kapsaması*
   - *kısa bir rapor; hiçbir şey değiştirilmez*
   *Açayım mı? İstediğin zaman `/purser:optimize` ile değiştirebilirsin.*

   For a paid OpenCode provider model, replace the first italic line in the matching language receipt with:

   EN: *When you are idle it runs read-only scans with your coder (<coder>), which uses its API credit, never during a job:*

   TR: *Sen boştayken kodlayıcınla (<coder>) yalnızca okuma yapan taramalar çalıştırır; bu, API kredisini kullanır; iş çalışırken devreye girmez:*

   On yes, add `--maintenance` to both plan and apply; on no, add `--no-maintenance` to both. After a successful apply, if the user opted into maintenance, immediately run the bundled binary's synchronous cleanup command: `"${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-${os}-${arch}${suffix}" maintenance run cleanup --repo "<repository-root>"`. It saves the report and cleanup `last-run.json` marker as part of the run, so the scheduler will not rerun cleanup for 56 hours. Read the report path printed by the command and present its findings in the same concise form as the SessionStart maintenance notice: finding count, the top five findings and evidence in a table, and the full report path (the full report retains any remaining findings). If the command fails, show the error in one ⚓ receipt and stop setup.
8. Only when `auto_compact_enabled` is `null`, regardless of coder, ask once in the user's language with one `AskUserQuestion` picker. Put On first and mark it “(recommended)” in the user's language, followed by Off. Keep `--auto-compact` / `--no-auto-compact` in both plan and apply; if it is `true` or `false`, do not ask again.

   EN: ⚓ **Automatic compaction** *When this conversation passes about 250K tokens, Purser saves a handoff (decisions, open work and the next step), runs /compact for you and picks the task back up by itself, so every later message costs far less and nothing is lost. It applies to all your projects. You can turn it off anytime with `/purser:auto-compact off`.*
   TR: ⚓ **Otomatik sıkıştırma** *Sohbet yaklaşık 250 bin tokenı geçince Purser bir devir notu kaydeder (kararlar, açık işler ve sonraki adım), senin yerine /compact çalıştırır ve işe kendiliğinden kaldığı yerden devam eder; sonraki her mesaj çok daha ucuza gelir, hiçbir şey kaybolmaz. Tüm projelerinde geçerlidir. İstediğin zaman `/purser:auto-compact off` ile kapatabilirsin.*
9. Run the bundled binary's `init --plan --coder X --reviewer AGENT`, adding the selected `--mode MODE` and manual `--model MODEL --effort EFFORT` only when Codex was chosen, or coder `--model MODEL` only when OpenCode or agy was chosen. For a selected Reviewer-1 model, add `--reviewer-model MODEL`; use `--reviewer none` when Off was selected. Pass the selected mechanical-route flag when OpenCode was detected. Add `--trust` and `--scaffold-profile` only if chosen. Include the selected maintenance flag for every chosen coder and the selected auto-compact flag when step 8 asked. Build a short summary from the `--plan` output and show it in one ⚓ receipt. Do not show the `normal` route. For small chores, show “Free OpenCode models” if step 4 was enabled; otherwise show the plan's mechanical route. Include the mode line only when a mode was chosen. Put other plan lines (trust, profile scaffold, maintenance, auto-compact, files to write) below as one short italic line each. Ask for confirmation in one yes/no `AskUserQuestion` picker, with Yes first and “(recommended)” in the user's language. Only after yes, run the same options with `--apply --json` instead of `--plan`.

   EN summary:

   > ⚓ **Purser is ready.** *Coder: {coder} · Reviewer-1: {reviewer} · Mode: {mode}*
   > - *Small chores → {mechanical model}*
   > - *Ordinary work → {subtle model / effort}*
   > - *Important work → {consequential model / effort}*
   > - *Plans → {plan_standard model / effort}*
   > *Shall I apply this setup?*

   TR summary:

   > ⚓ **Purser hazır.** *Kodlayıcı: {coder} · Reviewer-1: {reviewer} · Mod: {mode}*
   > - *Küçük işler → {mechanical model}*
   > - *Sıradan işler → {subtle model / effort}*
   > - *Önemli işler → {consequential model / effort}*
   > - *Planlar → {plan_standard model / effort}*
   > *Bu kurulumu uygulayayım mı?*

10. Run `/purser:check` and show its result in one ⚓ receipt.
11. Offer `/purser:optimize` in one sentence as an ⚓ receipt; do not run it automatically.

Never edit `~/.orchestrator/config.toml` or `.orchestrator/profile.toml` by hand. If detection or planning fails, show the error in one ⚓ receipt and stop setup.
