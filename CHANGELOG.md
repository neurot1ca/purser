# Changelog

Every plugin release (`.claude-plugin/plugin.json` version) gets an entry here
before it is published.

## 0.1.135 — 2026-10-12

- **Fixed:** A Codex Reviewer-1 receives the review schema, so its answer is no longer rejected as REVIEWER_RESPONSE_INVALID.
- **Fixed:** Job record writes retry a transient Windows replace failure for up to two seconds, so a job host no longer dies at start with HOST_DIED.

## 0.1.134 — 2026-10-12

- **Changed:** A Reviewer-1 from the same vendor as the implementer is no longer skipped, so any registered reviewer can be selected.
- **Fixed:** A Codex Reviewer-1 accepts Codex model ids and passes the configured reviewer effort to Codex.

## 0.1.133 — 2026-10-12

- **Fixed:** Asynchronous job finishes record the job's real run time instead of the result call's few milliseconds, so the duration-accuracy notice no longer reports absurd error rates.
- **Fixed:** Codex jobs without a worktree also get the shared, writable Go build cache.

## 0.1.132 — 2026-10-12

- **Fixed:** Replay git commands enable core.longpaths, so long paths no longer fail replay preparation on Windows.

## 0.1.131 — 2026-10-12

- **Fixed:** A codex call without a label writes a valid session ledger record, so its thread continuity and outcome are no longer lost (LEDGER_RECORD_INVALID).
- **Fixed:** Detected hand applies record an accepted decision, and send-back and redispatch decisions name the job that was decided.

## 0.1.130 — 2026-10-12

- **Fixed:** A codex_start that fails before launching a job no longer marks its task id finished, so the same task id can be started again.

## 0.1.129 — 2026-10-11

- **Fixed:** Codex workers get a shared, writable Go build cache under the Purser state root, so Go builds inside the sandbox no longer fail.
- **Added:** After three Reviewer-1 timeouts in a row a reviewer is skipped to its fallback for two hours, then probed again.
- **Added:** An implementation job that changes nothing is flagged IMPLEMENTATION_NO_DELTA and always gets a full review.

## 0.1.128 — 2026-10-11

- **Changed:** A job's ETA comes from this project's measured median for its route, so estimates correct themselves as jobs finish.
- **Fixed:** Briefs with a Required change, Done when or Definition of done section count as a grounded task contract and no longer force a full review.

## 0.1.127 — 2026-10-11

- **Changed:** Cleanup and test-gap notices list findings from most to least important and show a Severity column.

## 0.1.126 — 2026-10-11

- **Fixed:** Scheduled cleanup and test-gap reports run again; the quiet-period wait no longer leaves them reading a stale clock.
- **Changed:** The mechanical shadow check runs at most once every 70 delegated completions and logs when it starts and finishes.

## 0.1.125 — 2026-10-11

- **Fixed:** A regression test run red before the fix no longer marks the job's validation FAIL; runs after the job's last edit decide.
- **Fixed:** A finished job no longer holds its scope lock, a job applied by hand closes itself, and a queued job names the job it waits on.

## 0.1.124 — 2026-10-11

- **Fixed:** `check` no longer warns about the supported `[mechanical]` setting and finds a reviewer on PATH the way reviews do.
- **Fixed:** A manually registered codex-main server now gets the same post-call processing as the plugin one; unused config keys are gone.
- **Fixed:** The job waiter no longer sleeps past its time bound.

## 0.1.123 — 2026-10-11

- **Fixed:** Job results now really show the PASS or FAIL from Purser's own test capture; an async review step was still resetting it.
- **Fixed:** Replaying a finished job no longer times out on large build caches inside the job folder.

## 0.1.122 — 2026-10-11

- **Fixed:** A queued job is no longer overtaken by newer jobs whose scope overlaps it.

## 0.1.121 — 2026-10-11

- **Fixed:** The PASS or FAIL from Purser's own test capture is no longer reset to UNKNOWN when the reviewer finishes; a FAIL from either side wins.
- **Fixed:** The jobs board shows applied jobs as done and lists queued jobs under their task id.

## 0.1.120 — 2026-10-11

- **Fixed:** When Purser captures a job's test runs itself, the result now shows PASS or FAIL instead of UNKNOWN; the last run of each test command decides, so a test shown failing before the fix no longer marks the job failed.

## 0.1.119 — 2026-10-11

- **Removed:** The Python implementation and its tests; the plugin, release tooling and CI run only Go, so Purser no longer needs Python installed.

## 0.1.118 — 2026-10-11

- **Changed:** After a failed luna attempt, Purser routes the next attempt of the same task to normal (sol, medium) instead of consequential; a higher route you choose is kept.

## 0.1.117 — 2026-10-11

- **Added:** When a luna implementation attempt is rejected after its send-back or cancelled as failed, Purser routes the next attempt of the same task to consequential (sol, high) itself, with the failed attempt as basis.
- **Fixed:** A cancelled or discarded task id can start a fresh job instead of being refused as already finished.

## 0.1.116 — 2026-10-10

- **Fixed:** The reply-language instruction is part of every request, so turns started by background task notifications keep the user's language.
- **Fixed:** Worker test evidence counts only the latest run of each test target, so a passing rerun clears an earlier setup failure.

## 0.1.115 — 2026-10-10

- **Fixed:** A failed apply replay restores the worker's worktree, and hand-applied detection compares the preserved worker patch, so a job is never marked applied (and its worktree retired) from main's changes alone.
- **Fixed:** codex_apply registers newly created files with git (intent-to-add), so committing them by path works.
- **Fixed:** A worktree send-back whose scope overlaps another task's active lease queues like a start instead of being refused.
- **Fixed:** A plan no longer stays on "apply" after its linked jobs are discarded, so the Stop continuation resumes the next pass.
- **Fixed:** Host test evidence recognizes shell-wrapped, env-prefixed and `python -m pytest` test commands, so a worker's test run is recorded instead of rerun.
- **Fixed:** Git failures include a short stderr summary, and worktree ownership locks are written under the Purser state root instead of the repository.
- **Fixed:** Synchronous dispatch drains worker output before waiting, so the final usage event is not lost.
- **Fixed:** Discarded jobs show as discarded on the board, and cancelling a finished unapplied job returns a short receipt.

## 0.1.114 — 2026-10-10

- **Fixed:** Job signal servers read the whole message, so a PING or CANCEL split across TCP reads is no longer dropped (seen on macOS); older clients and hosts keep working.

## 0.1.113 — 2026-10-10

- **Fixed:** Applying a job closes earlier unapplied results of the same task, so superseded send-back rows no longer stay "in review" and their scope leases are released.
- **Fixed:** Jobs applied by hand after PATCH_UNAVAILABLE are detected and release their leases.
- **Fixed:** A cached tool result shows the current job board instead of the one from when it was first answered.

## 0.1.112 — 2026-10-10

- **Fixed:** Job patches and three-way replay no longer fail when a job deletes a file or adds one missing from the checkout.
- **Fixed:** The cleanup notice re-checks its findings, drops resolved ones, gives each a reason, trims evidence at a word and shows a report at most twice.
- **Fixed:** The Python host detects dead processes on macOS, and the reviewer fallback test no longer depends on the platform.

## 0.1.111 — 2026-10-10

- **Changed:** Plan milestone jobs show `<label> [<plan>-N/T]`, and applying a plain milestone job (also by hand) completes the milestone; the Stop reminder lists every open plan.
- **Changed:** Implementation jobs are no longer refused for having five or more scope paths; only consequential and escalated routes go to planning first.
- **Changed:** The board lists coded jobs still in result, review or acceptance above queued jobs.
- **Changed:** A Go release tool replaces scripts/release.py.
- **Fixed:** A confirmed-dead host fails its job as HOST_DIED before the deadline, and a failed admission baseline ends FAILED with its real cause instead of hanging as coding.
- **Fixed:** The acceptance revision hashes the job's patch, so it binds content and survives an incomplete admission baseline.
- **Fixed:** Replay stages only the job's paths, so it no longer times out on large checkouts.
- **Fixed:** The scoped manifest records ignored directories without walking them, and tests no longer leave builds in go/.tmp.
- **Fixed:** Fetching a result no longer flashes a console window.

## 0.1.110 — 2026-10-10

- **Changed:** The job host records the worker's test runs (command, exit code, output tail) and validation comes from that evidence, so Claude no longer reruns focused tests that already passed.
- **Changed:** The band and the job board name each job's stage (coding, coded · fetching result, in review, accepted, done) and no longer show overdue.
- **Fixed:** A job whose worker died is finished by wait (TIMED_OUT past its deadline), and cancelling it gives CANCELLED instead of PROCESS_UNCONFIRMED.
- **Fixed:** Scope leases no longer leak: hand-applied jobs, superseded send-back jobs, cancelled queued jobs and unconfirmed launches release theirs.
- **Fixed:** The Linux test suite and Go parity tests pass on Linux.

## 0.1.109 — 2026-10-09

- **Fixed:** Codex jobs launch on Linux and macOS instead of failing with PROCESS_UNCONFIRMED.
- **Fixed:** The job waiter stops after about 9 minutes with STILL_WAITING and is rerun, so waiter processes no longer pile up.
- **Fixed:** A failed async finalization releases the job's scope lease, and a replay-preparation failure is retryable with its cause instead of a terminal INTERNAL_POST_ERROR.
- **Fixed:** plan complete-milestone is blocked only by a running linked job of that milestone and names it.
- **Fixed:** check, init, optimize, the hooks and the auto-compact, board and usage mods honour CLAUDE_CONFIG_DIR and PURSER_STATE_DIR instead of a fixed ~/.claude.
- **Fixed:** Cleanup, retirement and hand-resolved apply compare canonical paths, so short-name and symlinked temp roots work.
- **Added:** Release packaging builds a six-target plugin ZIP with checksums, a tagged release workflow and a distribution smoke harness.
- **Changed:** The test suites run on Linux and macOS CI as well as Windows.

## 0.1.108 — 2026-10-09

- **Fixed:** Hooks find the plugin root from the installed binary, so `codex_start` works outside the Purser repository (found testing on Linux).
- **Fixed:** A result call after a stale-apply refusal re-prepares the job, so the refreshed revision can be accepted and applied.
- **Fixed:** An OpenCode tool error, including a denied permission, is recorded as a job note instead of failing the whole job.

## 0.1.107 — 2026-10-09

- **Fixed:** A Reviewer-1 fallback runs its own CLI instead of the primary reviewer's configured executable.
- **Fixed:** An explicit valid Codex model on a new mechanical call wins over the OpenCode candidate.
- **Fixed:** A plain `Requirements:` line grounds the task contract like `Acceptance criteria:`.

## 0.1.106 — 2026-10-09

- **Fixed:** A plan's complete-milestone is no longer blocked by a collected no-change or discarded job.
- **Fixed:** A reply on a job without a worktree launches in that job's recorded repository root.
- **Fixed:** The session ledger no longer rejects a record whose keys are repository paths containing words like "token".
- **Changed:** A failed Reviewer-1 launch records the resolved executable, argument flags, cwd and PATH head for diagnosis.
- **Removed:** pilot-report, measures-sync, the one-off evaluation, token report, migration and report scripts, and the unread shadow telemetry events.

## 0.1.105 — 2026-10-09

- **Added:** `/purser:check` shows a Hook activity row: when each hook event last ran, recorded at most hourly.
- **Changed:** Implementation and operation jobs run through a fresh `purser:dispatcher` agent by default; main Claude keeps review, acceptance, and commits.

## 0.1.104 — 2026-10-09

- **Fixed:** Reviewer-1's claude fallback gets a valid effort, so a failed agy attempt no longer ends every review as unavailable; each unavailable attempt keeps its reason.
- **Fixed:** codex_start without `cwd` launches in the repository root and refuses with a clear error outside a repository.
- **Fixed:** An empty or omitted inline plan summary names the complete plan file.
- **Fixed:** The Go hook uses the configured bundle size budget (default 8192 bytes), as Python does.

## 0.1.103 — 2026-10-09

- **Added:** A job's worktree and preparation are removed in the background right after apply, discard, cancellation or a no-change finish; a later reply starts on a fresh worktree.
- **Added:** Several plans can be active in one project; plan commands take `--task-id`, and Stop names every plan that needs its next step.
- **Changed:** Automatic /compact is off unless `/purser:init` records a yes.
- **Changed:** Leftover worktrees and job records are cleaned after 3 days instead of 7.
- **Changed:** The dispatcher waits in the foreground until its job finishes and reads only what it is given.
- **Fixed:** pilot-report charges a warm dispatcher only for its own job's requests.

## 0.1.102 — 2026-10-09

- **Fixed:** Maintenance no longer treats RUNNING/QUEUED jobs past their deadline as live work.
- **Fixed:** Transcript reading seeks to the last 4 MiB instead of loading the whole file.
- **Fixed:** Lease conflict reports name the first stored owner.
- **Fixed:** The plan Stop reminder stays quiet while a job for the plan's task, including a reply job, is running.
- **Fixed:** A reply on an already-applied worktree finalizes instead of failing replay preparation.
- **Fixed:** A reply launch inherits a missing repository root and worktree path from the prior thread job.
- **Fixed:** A finished job's thread keeps its task, route, model and effort, so a reply to it is admitted.
- **Fixed:** Reviewer-1 findings reach the bundle whatever list type they arrive in.
- **Changed:** The plan Post hook names every missing required plan section.
- **Added:** The new-model notice also announces the newest catalog model in usable families; the catalog refreshes hourly.
- **Added:** The Go hooks offer a Codex CLI update once per new version.

## 0.1.101 — 2026-10-09

- **Fixed:** The plan Stop hook no longer asks to dispatch while a `purser:dispatcher` agent runs the pass.
- **Fixed:** Recording an acceptance no longer reports "invalid helper output" from the Post hook.
- **Added:** `purser-hook pilot-report --job <id>` reports a job's per-arm usage, timing, prompt response times and optional API-equivalent cost.
- **Added:** A failed Reviewer-1 attempt keeps its raw output in `diagnostics/reviewer` (newest 20) so the failure can be diagnosed.

## 0.1.100 — 2026-10-09

- **Fixed:** The acceptance gate now sees which subagent called `codex_apply`, so a dispatcher can no longer apply a job main Claude has not accepted.
- **Fixed:** The Python audit receipt's commit command also lists every changed path.

## 0.1.99 — 2026-10-09

- **Fixed:** The audit receipt's commit command lists every changed path instead of only the first 10.
- **Fixed:** A dispatcher agent's `codex_apply` now really waits for main Claude's acceptance; the check also refuses it if the gate itself fails.

## 0.1.98 — 2026-10-09

- **Fixed:** Replying on a Codex thread works again after new commits land; the reply uses the job's recorded repository instead of failing with WORKTREE_SNAPSHOT_FAILED.
- **Added:** Codex jobs started by the dispatcher agent are tagged with its id, and its contract records main Claude's acceptance with `codex_apply acceptOnly` (pilot groundwork).

## 0.1.97 — 2026-10-08

- **Fixed:** The Claude Haiku reviewer returns a valid review: the prompt is no longer swallowed by the `--tools` flag.
- **Fixed:** Cleanup now actually reclaims old job worktrees past the size cap, while keeping running, conflicted and recently finished unapplied jobs.
- **Fixed:** After auto-compact, the notice arrives as Purser's own message in the desktop app, where transcript lines were not shown.
- **Added:** Purser keeps only the running and previous plugin versions in the plugin cache, freeing about 150 MB per old version.
- **Added:** A background dispatcher agent can apply a Codex job only after main Claude accepts that exact revision (pilot groundwork).

## 0.1.96 — 2026-10-08

- **Fixed:** Reviewer-1 no longer comes back unavailable when Gemini stalls: Claude Haiku, then Codex, review in its place.
- **Changed:** The gear-up suggestion appears 90 minutes before the 5-hour reset, in the same style as the Codex done line.
- **Changed:** Maintenance reports run up to three times a week after 5 quiet minutes, and `/purser:init` runs the first one right after you opt in.
- **Changed:** Maintenance notices say why each finding matters and how to turn the reports off.
- **Changed:** Auto-compact announces itself as a Purser line in the transcript, and the context advisor no longer asks you to type `/compact` while it is on.

## 0.1.95 — 2026-10-08

- **Fixed:** Purser's state folder no longer grows without bound: finished job worktrees and preparations are removed after 7 days, and the folder is capped at 2 GiB.
- **Fixed:** An accepted OpenCode job counts as accepted, so shadow sampling of other models starts.
- **Changed:** The Codex quota popup shows only the quota line; apply guidance goes to the model instead.
- **Changed:** Implementation and plan prompts carry the in-scope code map excerpt instead of a path.
- **Changed:** Plan jobs get the required plan shape from Purser; plans without it are flagged.
- **Changed:** Maintenance reports scan every project language with its installed tools; the coverage target is configurable (`[maintenance] coverage_target_pct`, default 90).
- **Changed:** The auto-compact wake message shows how much cheaper later messages are.

## 0.1.94 — 2026-10-08

- **Fixed:** Mechanical OpenCode jobs start again; `PURSER_CODEX_BIN` applies only to Codex.
- **Fixed:** No console window flashes on Windows when Purser starts git or helper processes (codex_start, codex_result, tests).
- **Fixed:** Reviewer-1 token usage is recorded again.
- **Changed:** The auto-compact wake message is a bold Purser line naming `/compact`; the expected headless refusal no longer shows in the transcript.

## 0.1.93 — 2026-10-08

- **Changed:** Purser no longer needs Python: `/purser:check` runs in Go with a Runtime row, the MCP launcher only starts the Go binary, maintenance reports show the Runtime line, and released plugins ship no Python files.
- **Added:** Auto-compact wakes the session after `/compact` with the measured saving so the task in progress continues.
- **Fixed:** A rejected `codex_start` names the underlying launch error.

## 0.1.92 — 2026-10-08

- **Fixed:** Auto-compact runs the `/compact` command where the host refuses plugin compaction.
- **Changed:** `/purser:init` no longer looks for Python or stores its path.

## 0.1.91 — 2026-10-08

- **Fixed:** Auto-compact works where the host refuses plugin compaction (such as the desktop Code tab) by submitting a `/compact` prompt instead.

## 0.1.90 — 2026-10-08

- **Added:** An accepted plan keeps moving: Purser records it, restores where it stopped after compaction or a new session, and reminds Claude to dispatch the next step; only an explicit `/purser:plan stop` pauses it.
- **Changed:** The code map is built natively for 17 languages (C# newly covered) and completes itself in the background; Pre no longer runs Python.
- **Changed:** Maintenance reports and their commands run through `purser-hook maintenance`; SessionStart no longer launches Python.

## 0.1.89 — 2026-10-08

- **Fixed:** Auto-compact retries on timers that outlive the finished turn and logs each decision to `~/.claude/purser/auto-compact.log`.
- **Changed:** Mechanical shadow runs start through `purser-hook` instead of Python.

## 0.1.88 — 2026-10-08

- **Added:** After a main turn at 250k context tokens Purser compacts the conversation itself, so you no longer type `/compact` (`PURSER_AUTO_COMPACT_TOKENS` sets the threshold, `0` turns it off).

## 0.1.87 — 2026-10-08

- **Added:** Claude can be the primary coder and planner; each tier (Haiku, Sonnet, Opus) always runs its newest model, and `/purser:init` sets it up.
- **Changed:** New jobs and Reviewer-1 run the newest model of the configured family from local caches; a newer model is reported once instead of asking, and the adopt/decline model flags are gone.
- **Added:** A new implementation call with five or more non-test scope paths or a consequential/escalated route is sent to `/purser:plan` first, unless it belongs to a completed plan.
- **Changed:** The `/purser:init` intro mentions parallel worktree jobs with queueing and plans offered when a task needs one.

## 0.1.86 — 2026-10-07

- **Changed:** `/purser:init` runs in Go with the same detection, plan and model-version decisions.
- **Fixed:** A Codex job without `cwd` runs in the repository root instead of failing on an empty `--cd`.
- **Fixed:** An async result that cannot be finalized ends FAILED and logs its real cause, so Stop stops asking for it; a reply on an applied worktree finalizes its own delta.
- **Fixed:** Finished jobs with no delta or a terminal failure release their scope lease; leases of terminal jobs whose worktree is gone are released at admission.
- **Fixed:** Operation jobs record the Reviewer-1 phase skip instead of a false UNCERTAIN; CHANGES_REQUIRED without a finding is rejected; an unavailable Reviewer-1 keeps bounded stdout/stderr/exit evidence in telemetry.

## 0.1.85 — 2026-10-07

- **Changed:** Sol routes (normal, consequential, escalated, plan) default to gpt-6.1-sol; research jobs go on the normal route.
- **Added:** Claude can serve as the fallback coder and as Reviewer-1 with its own model and effort; a same-family Claude reviewer is skipped.
- **Changed:** `/purser:optimize` runs in Go, reports disabled plugins as stopped and restores its diagnostics.
- **Changed:** Plans and plan summaries are stored complete; a summary too large for the bundle points to the plan file. Async plan jobs report `plan_path` too.
- **Fixed:** A newer Codex model is detected at job start again, not only at session start.
- **Fixed:** Git and Python-locator processes launched by Purser no longer open console windows on Windows.
- **Fixed:** The job board wraps below the usage band on narrow windows instead of drawing over it.

## 0.1.84 — 2026-10-06

- **Added:** `/purser:gear-up` and `/purser:gear-down` (or just ask in chat) switch a project's luna-high Codex work to the configured sol model at medium until geared down; Pre enforces it for new jobs.
- **Added:** After a Codex job, Purser suggests gearing up when quota would otherwise go unused, or back down when it runs low, at most once per quota window; receipts show `gear up`.
- **Added:** The usage band shows Claude's context fill and the latest Codex job's model and effort.
- **Fixed:** Lock files are no longer written before locking, removing a Windows lock race.

## 0.1.83 — 2026-10-06

- **Fixed:** Reviewer-1 gets its configured 180 s timeout again instead of a silent 90 s default; the Go Post hook now honors numeric config values (hook budgets, report limits).
- **Fixed:** The band job board no longer triggers an engine refusal when another plugin draws above the prompt.
- **Fixed:** Model-version discovery checks routes in config order, matching Python.

## 0.1.82 — 2026-10-06

- **Changed:** Python is no longer needed for core use: the codex-main MCP server, the job waiter, scope leases, SessionStart and MCP-start maintenance, Reviewer-1 and the readiness check all run natively in Go; the Python MCP fallback and the Python SessionStart hook are removed.
- **Added:** The Go MCP host refreshes the models.dev catalog cache at every start when it is older than 24 hours.
- **Changed:** Opt-in maintenance reports still use Python and are skipped with a notice when Python is missing.

## 0.1.81 — 2026-10-06

- **Fixed:** The plugin upgrade note and the free-model ranking notice appear again at session start under the Go hooks, and the daily pinned model-version discovery runs again.
- **Fixed:** codex_apply releases an applied job's scope lease even after its worktree was retired.
- **Added:** The detached Go job host writes its errors and recovered panics to diagnostics.jsonl.

## 0.1.80 — 2026-10-06

- **Fixed:** Under the Go MCP host, codex_cancel waits for the job's terminal state instead of reporting it unconfirmed, and codex_apply releases the job's scope lease so later overlapping jobs no longer queue behind applied ones.
- **Fixed:** The codex_start waiter command points to the plugin's waiter, not a path under Claude's current directory.
- **Fixed:** A job whose Go host died after Codex finished is recovered as a failed job instead of staying running forever.

## 0.1.79 — 2026-10-06

- **Fixed:** codex_start works again under the Go MCP host; the detached host launch no longer fails with access denied when attaching to the desktop shell parent.

## 0.1.78 — 2026-10-06

- **Changed:** codex-main now runs on the Go MCP host (`purser-hook mcp`), sharing the private CLI's operations, with per-request cancellation and a background startup recovery scan; the launcher falls back to the Python server only when the Go process cannot be created.
- **Fixed:** codex_cancel from the Python server confirms the terminal state of a Go-hosted job instead of reporting it still running.

## 0.1.77 — 2026-10-06

- **Fixed:** Codex task records written with whole-number floats are no longer reported as a corrupt ledger by the Go hook, and reusing a finished task id is denied plainly as TASK_ALREADY_FINISHED.
- **Fixed:** The worker full-suite guard fires only on an explicit run/execute request and names the matched phrase; the Go waiter command uses an absolute path.
- **Fixed:** When the Go Post/Failure helper fails, the mod keeps the result but tells Claude a FULL review is required, and a failed toast no longer drops enforcement output.

## 0.1.76 — 2026-10-06

- **Fixed:** Purser's state no longer grows without bound: job cleanup lists git worktrees once per repository, retires finished jobs' worktrees and replay backups, and prunes expired job records with their raw outputs and bundles after distilling them into a learning summary capped at 4 MiB; every state directory has a registered retention policy and a total-size check.

## 0.1.75 — 2026-10-05

- **Fixed:** codex-main connects again; its startup recovery scan runs in the background instead of delaying the MCP handshake past the connect timeout.

## 0.1.74 — 2026-10-05

- **Fixed:** `codex_result` and `codex_cancel` no longer report "routing was NOT enforced" when Go PreToolUse allows the call without changes.
- **Fixed:** Go PostToolUse/PostToolUseFailure get 300 seconds again; the 60-second limit killed Reviewer-1 finalization and returned unreviewed results.
- **Internal:** Go hook binary gains private `op`/`host` entry points and recovery replacement; still not wired.

## 0.1.73 — 2026-10-05

- **Fixed:** The mod adapter runs the Go PostToolUse and PostToolUseFailure helper in enforce mode; in 0.1.72 Go skipped Post enforcement (review, receipts, apply refresh) for the Codex tools.
- **Internal:** Go `jobs` package adds job start for replies, finish/result, cancellation, apply with hand resolution, queued admission, worktree creation and setup, readiness, and recovery classification and replacement, each against a Python oracle; still not wired.

## 0.1.72 — 2026-10-05

- **Changed:** Every Purser hook for the Codex tools now runs in Go through the mod adapter: async `codex_start`/`codex_result`/`codex_cancel` PreToolUse, PostToolUse, PostToolUseFailure and Stop. The Python command hooks are no longer registered by the plugin; the Python bridge and Reviewer-1 bridge still run as subprocesses.
- **Added:** The measurement ledger and its reports are projected into the Obsidian vault (`purser-hook measures-sync`, also at SessionStart).
- **Fixed:** Obsidian projection settings resolve from the plugin root before the working directory.
- **Internal:** Go `jobs` package ports the Codex bridge's job store and locking, thread records, launch arguments, alternate agents, event parsing, Windows/POSIX detached launch and containment, the host signal protocol and liveness, each against a Python oracle; nothing is wired to it yet.

## 0.1.71 — 2026-10-04

- **Changed:** PreToolUse for `codex` and `codex-reply` runs in Go through the mod adapter instead of the Python hook; `codex_start`, `codex_result`, `codex_cancel` and every Post/Stop hook stay Python.
- **Fixed:** If the Go Pre adapter fails after Go wrote routing state, the call is reviewed FULL and its scope lease and pending record are released instead of being treated as routed.
- **Fixed:** A fix reply starts from the latest unapplied job of its thread, so commits on main no longer turn replies into replay conflicts.
- **Added:** Telemetry records Reviewer-1 valid/false counts at apply, replay conflicts, the longest silent gap, full-suite denials and the scope path count; a prompt saying "no full suite" is no longer denied.
- **Internal:** More Go hook code (fallback questions, quota-renewal offer, admission, task usage, projections, recovery) is ported; process contracts run Go Pre against the Python Post, Failure and Stop hooks.

## 0.1.70 — 2026-10-04

- **Fixed:** Desktop usage pills size to their content and draw as images, so they no longer sit in large white boxes and labels no longer overlap the bar.
- **Fixed:** After a manual /compact, compaction advice no longer repeats the pre-compact token count and banner on every prompt.

## 0.1.69 — 2026-10-04

- **Fixed:** The band above the prompt disappeared on the desktop app because a usage pill used an invalid drawing property; the pills now draw.

## 0.1.68 — 2026-10-04

- **Changed:** The job board above the prompt is one summary line (running, queued, done, next ETA); a details button opens the per-job table in a pane.
- **Added:** An alternative usage band shows Claude and Codex 5h/7d quota pills and the session cost; a pill without a data source is hidden.
- **Fixed:** Compaction advice, Codex retry context and the maintenance notice are back on every prompt; they were lost when the prompt hook moved to Go in 0.1.65.
- **Changed:** Implementation and plan prompts get the code map line automatically; the map skips oversized and binary files.
- **Internal:** More Go hook code (enforcement, leases, review, ledger) is ported but not yet active.
## 0.1.67 — 2026-10-04

- **Added:** The Purser job board is drawn above the prompt by the mod, with elapsed and remaining time, overdue, queue wait and last agent activity; it costs no Claude tokens. Chat receipts stay for now.
- **Fixed:** A reply moves its thread worktree to the current HEAD instead of copying the checkout over an old commit, so replies no longer fail with replay conflicts on other jobs' files.
- **Fixed:** `codex_apply resolved=true` closes a job that failed on a worker replay conflict and records it as accepted.

## 0.1.66 — 2026-10-03

- **Fixed:** Prompts with `Goal:` and `Acceptance:` headers count as a grounded task contract, so the readiness check runs and Reviewer-1 is not forced to FULL review.
- **Fixed:** Context turn events carry the session id, so the count of Claude turns during a job is real (0 when idle, unknown only without tagged turns).
- **Added:** Telemetry records readiness verdicts, worker prompt size, how scope leases end (applied, resolved by hand, discarded, expired) and the hook runtime.

## 0.1.65 — 2026-10-03

- **Changed:** SessionStart, UserPromptSubmit, PreCompact and the context Stop hook run from the Claude Code mod on the Go binary; any Go failure lets the event continue. Python stays only for job-worktree cleanup at session start.
- **Added:** A readiness check asks a second agent (agy by default, `[coding.readiness_check]`) whether the job meets its acceptance criteria and records the verdict on the job; it never blocks the job.
- **Added:** At dispatch, Purser offers a newer Codex model or CLI once per version.
- **Added:** Telemetry records Codex token usage, Claude turns that finished while a job ran, readiness verdicts, worker prompt size, how scope leases end, and whether a hook ran on Go or Python.
- **Fixed:** A reply no longer fails with a replay conflict when the thread's earlier changes are already in main.
- **Fixed:** The job board shows a job waiting on a scope lease as "queued behind <task>".
- **Added:** The pre-hook denies a worker prompt that asks for the full suite when the project says Claude runs it at acceptance.
- **Changed:** Each finished job gets one result line outside the 5-line reply limit.
- **Fixed:** `codex_cancel` on a finished, unapplied job discards it and frees its scope lease.

## 0.1.64 — 2026-10-03

- **Changed:** The Read hook runs from a Claude Code mod (`hooks/register.ts`) that calls the Go binary; Read always proceeds if the helper fails. Requires Claude Code 2.1.287 or later.
- **Fixed:** Session maintenance no longer deletes a worktree while another job sharing it is running or unapplied.
- **Fixed:** Unix Go hook binaries ship executable, and the launcher restores the exec bit once before falling back to Python.
- **Changed:** Replies follow ASD-STE100 sentence rules within the 5-line limit and may exceed it only to state a risk, failure or pending decision.
- **Changed:** Purser is dual-licensed under MIT OR Apache-2.0.

## 0.1.63 — 2026-10-03

- **Changed:** The Read hook runs as a native Go binary built for Windows, Linux and macOS (amd64/arm64); Purser falls back to Python when the binary cannot start.
- **Fixed:** A Codex reply after the previous job was applied and committed no longer fails with a replay conflict.
- **Added:** `reason=fix` for replies that send work back after review, and the worker's PASS/FAIL validation in job telemetry.
- **Removed:** Unused internal code flagged by the cleanup report.

## 0.1.62 — 2026-10-02

- **Changed:** Codex receipts, the job board and lifecycle notices (maintenance, upgrade, new model, free-model leader, fallback) now reach Claude before it replies, so it writes them in its first reply; the Stop hook only blocks as a fallback when a line is missing, saving an extra full-context request each time.
- **Added:** Stop blocks emit STOP_BLOCKED telemetry (reason kinds and count, no notice text).
- **Changed:** Maintenance `mark-done` takes `--acted <path_line>` values and records acted versus listed findings.
- **Changed:** The compact Codex result no longer repeats the worker's VALIDATION line.

## 0.1.61 — 2026-10-02

- **Fixed:** The handoff's recent history no longer shows stale turns from before a compaction.
- **Added:** `/purser:init` asks whether to enable background maintenance (cleanup reports), and maintenance reports record finished and acted-on telemetry.
- **Added:** Outcome telemetry for Reviewer-1 finding counts, compaction advice, OpenCode shadow runs and scope conflicts, plus per-job code-map use rows in the code-map report.
- **Changed:** Removed six unused imports found by the first cleanup report.

## 0.1.60 — 2026-10-02

- **Fixed:** The async Codex bridge works on Linux: finished jobs are detected, dead hosts recover, and recovery waits until the old worker is gone.
- **Fixed:** Reviewer-1 finds agy, OpenCode or Codex on PATH on any OS (an absolute `[coding] reviewer1_executable` still wins), and configuration errors name their cause.
- **Fixed:** Transcript token attribution no longer raises a false identity mismatch on Linux.
- **Changed:** The Obsidian vault comes from `PURSER_OBSIDIAN_VAULT` or config and must be absolute; nothing is written when it is unset.
- **Changed:** The `/purser:plan` dispatch receipt no longer states a stall threshold.

## 0.1.59 — 2026-10-01

- **Added:** The daily new-model notice also checks the Reviewer-1 Gemini model; adopting it changes only the reviewer model.
- **Changed:** Every user-facing Purser notice has an English master that Claude shows in the user's language.
- **Added:** Maintenance can run a read-only test-gaps report with evidence-bound candidate tests, and shows a coverage milestone message above 90% line coverage.
- **Fixed:** An explicitly requested model such as gpt-6-sol is now accepted for a new Codex call.

## 0.1.58 — 2026-10-01

- **Changed:** An explicitly requested model and effort on a new Codex call now win over the route defaults; replies, fallback and the mechanical OpenCode path keep their own models.
- **Fixed:** Telemetry reports read the global task event index and count starts with no project in a separate global bucket.
- **Added:** An admitted sync reply records a send-back decision, and the route outcome report shows failed, cancelled, timed-out and awaiting-apply jobs apart from Claude's decisions.
- **Changed:** The delegation skill states the Objective plus Acceptance criteria prompt form that grounds the task contract.

## 0.1.57 — 2026-10-01

- **Fixed:** A reply chain applies against the thread's first baseline, so continued jobs no longer fail with a worker replay conflict.
- **Added:** A silent running job is marked stalled once per silence episode from learned per-route thresholds without being cancelled; the waiter reports it once and the board shows it as stalled. Async jobs default to a 24-hour cap.
- **Changed:** The Codex done receipt also shows the time until the 7-day quota resets.
- **Added:** The route outcome report attributes results to the served route, counts rejected and sent-back attempt cost per accepted result, and uses only route-time task types.
- **Fixed:** Telemetry records the route-time task type, the served model, effort and route, and an accepted decision on a repeated apply of a reply chain.

## 0.1.56 — 2026-09-30

- **Added:** An opted-in project can run a read-only Codex cleanup report by hand or automatically when idle (quiet period, no user job, weekly cadence, one global slot); findings are ranked and one notice offers a 'temizle' handoff that lands the cleanup as one revertible commit.
- **Added:** Maintenance uses the 5h or weekly quota window, asks API-billed users before scanning, shows the report table at 70% and again at 30% remaining quota per window unless skipped, and can be turned off or run from /purser:optimize.
- **Added:** At most once a day SessionStart offers a newer same-family version of a pinned route model and switches only on the user's yes.
- **Added:** Each finished job's task record carries has_code_map so the code map saving can be measured.
- **Changed:** /purser:optimize renders a Purser-style quoted table with each source's status and last-seen date, lists already stopped sources on one line, and is shown in the user's language.
- **Changed:** The planning nudge shows once per session and the language line only at session start, after compaction or clear, on a language change, or every 20 prompts.
- **Changed:** The orchestrator skill dispatches overlapping worktree jobs immediately; Purser queues hot-path conflicts and runs the rest in parallel worktrees.
- **Fixed:** Replay applies a worker delta directly whenever it applies cleanly to the current checkout, so unrelated main changes no longer fail good jobs as conflicts.
- **Fixed:** Reviewer-1 telemetry falls back to the measured elapsed time when the report's elapsed_seconds is missing or invalid.

## 0.1.55 — 2026-09-30

- **Changed:** Purser messages, dispatch and done receipts and the jobs board render as a grey quote with an anchored bold first line; the stop hook accepts both the quoted and the legacy form.
- **New:** running Codex jobs record the active agent, last activity, a process-alive checkpoint at most once a minute and the longest silent gap, and a stale generation cannot overwrite a newer record.

## 0.1.54 — 2026-09-30

- Operation and plan jobs record Claude's acceptance when their result is read, and redispatching a task records the earlier unapplied job as rejected.
- Setup offers once to turn on the Concise reply style, changes only `outputStyle` on yes, and points to `/output-style concise` when the write is not allowed.
- Telemetry records a conservative derived task type and flags a suspected route mismatch without changing dispatch.
- Task telemetry keeps selected and served agent, provider and model, Codex turn usage and the latest Claude decision; Reviewer-1 failures record an error code, elapsed time and retry outcome.
- The route outcome report shows the distribution by task type, selected versus served agent, decisions, Reviewer-1 outcomes, Codex tokens and suspected mismatches.

## 0.1.53 — 2026-09-30

- A replay whose paths are all new files is not a conflict, and a replay conflict keeps the job's thread replyable.
- A reply job shows applied on the board when its thread's apply covered every path of its delta.
- Job durations start when the job starts, not when it is queued, and codex and codex-main history pool together for ETAs.
- The handoff's mechanical state carries an as-of stamp and source tags, and Claude ends each State and Decisions line with its source.
- A job worker no longer rewrites a job's metadata after it was cancelled.
- When Reviewer-1 saw only part of a delta, Claude's bundle lists reviewed and omitted paths and forces FULL review.
- Plan milestone jobs use a `<plan>-m<N>of<T>` task id, and the handoff shows one line of milestone progress and the next step.
- The test guard also blocks detached shadow runs, and two timing-dependent tests are deterministic.

## 0.1.52 — 2026-09-30

- A job lease whose job file is gone is released after ten minutes, so it no longer blocks the queue.
- Seven dead symbols are removed, and context I/O telemetry reuses telemetry's atomic JSON writer.
- A test covers a fallback override whose consent came from an earlier session.
- A benchmark that makes a free OpenCode model the first candidate announces it as the mechanical leader.
- The OpenCode benchmark worker scores every candidate in one run, resumes at startup, logs each run, and holds a heartbeat lock.
- A blocked Stop tells Claude to write only the missing lines instead of repeating its reply.

## 0.1.51 — 2026-09-29

- The job board shows only while a job is active or newly finished, finished rows leave after one hour, a queued job shows no ETA, and a running job's ETA counts from its real start.
- Claude adds the code map line only when the task or planning goal lacks file or symbol locations.
- The code map use report counts map reads in shell commands nested inside functions.exec.

## 0.1.50 — 2026-09-29

- On Windows Reviewer-1 starts agy in a hidden new console, so console grandchildren no longer flash a window.
- The job board retires finished jobs without a worktree after two displays, shows research and plan rows as review skipped, and marks a reply row applied when its thread was applied.
- A PreCompact hook refreshes the handoff's state block before compaction.
- The Stop hook retries a finished uncollected job or a denied Codex call at most twice per turn, then the next prompt carries the unfinished step.
- The OpenCode mechanical benchmark lets the model edit its scratch files, and provider outages are not recorded as scores.
- Task projections record Purser-derived task types and Claude acceptance decisions with source provenance.
- An on-demand route outcome report groups tasks by route, model, effort and task type with acceptance, send-backs and separate worker and Claude token distributions.
- An on-demand report measures eligible Codex code-map reads and marks other providers without traces as unobservable.
- Reviewer-1 records a fingerprint for consistent identifier renames (no raw diff) and an on-demand report lists repeated mechanical edits.

## 0.1.49 — 2026-09-29

- Parallel worktree jobs conflict less: conflict-prone paths are remembered and serialize overlapping jobs, a finished job is replayed onto the current checkout before review with one in-thread conflict resolution, codex_apply refuses a drifted reviewed diff and re-prepares it, applied or unapplied-for-two-hours jobs release their leases, and resolved:true records a hand-resolved conflict; free OpenCode models are scored by a bundled mechanical benchmark on candidate refresh, and a failed free job with proof of no work is taken over once by Codex; fallback cycles are tested end to end.

## 0.1.48 — 2026-09-29

- Purser returns from the fallback coder to the primary by approved rules, Stop accepts required lines written anywhere in the turn, the /compact suggestion appears as soon as the context threshold is crossed, a job is marked overdue only after the route p90, and reply jobs already applied with their thread no longer show as conflicts.

## 0.1.47 — 2026-09-28

- When the primary coder is near its quota or fails with a typed error, Purser asks once to switch to the fallback coder and keeps later new jobs on it; an explicit codex_start ETA is kept; only jobs finished after this release appear on the board, each for two displays.

## 0.1.46 — 2026-09-28

- Worktree jobs can write anywhere in their worktree, mechanical calls with a skipped reviewer can start free-model shadows, routes without history get a default ETA, hook subprocesses no longer flash console windows, a finished job leaves the board after two displays, init offers an optional fallback coder, and failed coder launches are classified (credit, rate limit, no response) with safe retries.

## 0.1.45 — 2026-09-28

- A new leader among measured free mechanical models is announced once at the next session start, and the job board shown after codex_apply marks the job as applied.

## 0.1.44 — 2026-09-28

- Free OpenCode models now start measuring: detection skips non-standard model ids, and accepted Codex mechanical runs also launch shadow runs.
- Stop advice without receipt lines asks only for the job board, codex_apply refreshes a pending board, the receipt ETA follows the job ETA, and a stale shadow lock is reclaimed.

## 0.1.43 — 2026-09-28

- The first session after a plugin update announces the new version once, summarized from its changelog entry.
- Free OpenCode models for mechanical work are measured in the background: accepted runs and one detached shadow run per project record per-model results, and the ranking reorders after five samples.

## 0.1.42 — 2026-09-27

- A reply job takes a fresh snapshot of its reused worktree on first launch, so its patch carries only the reply's own changes and applies without conflicts.
## 0.1.41 — 2026-09-27

- The job board shows one row per Codex thread, in-progress jobs plus the three most recently finished, and marks jobs without changes as `no change`; results and applies no longer re-demand the dispatch receipt.
- The worker contract asks for focused tests, the smallest change and skipping unrelated hanging tests; `review.full_suite_required` in a project profile asks for one full-suite run instead.
- The OpenCode Reviewer-1 confines tools with `ask`, which OpenCode Zen free models accept.
## 0.1.40 — 2026-09-27

- Dispatch receipts and the job board are required in the next reply through the Stop hook, and the board shows only labelled, live or recently finished jobs.
- Init lists Reviewer-1 options by model with same-family marks, accepts OpenCode or Codex reviewers, offers free OpenCode models for mechanical chores, and finds the platform OpenCode binary.
- A fresh start with reason `user_override` or `reanchor` is allowed beside a compatible live thread; reply denials name the caller and the mismatched field.
- Quota receipts older than an hour use the stale variant, and the handoff skips harness system notifications.
## 0.1.39 — 2026-09-27

- Purser writes the dispatch receipt itself from a required short task label; with two or more jobs in flight it adds a job board (task, model, state, remaining ETA, review, apply).
- OpenCode and Codex can serve as Reviewer-1 through read-only adapters, chosen with the configured order and the vendor-family rule.
- `codex_apply` reports only the files that failed, lists the ones that would apply, leaves caches out of job patches, and tries a 3-way merge before reporting a conflict.
- A job's delta, scope check and review diff come from its own worktree snapshot; changes made outside the job are flagged instead of attributed to the worker.
- A job started from a folder missing in its worktree runs at the worktree root, and a failed Codex launch reports its cause.
- No console window flashes during `codex_result` on Windows.
- The Codex token report splits totals by project.

## 0.1.38 — 2026-09-27

- Async implementation jobs run in their own git worktree built from your current tree, so parallel plan milestones no longer wait on each other; review and checks run there, and `codex_apply` applies the reviewed patch once.
- A trusted project profile's `[worktree]` section can copy ignored files and run a setup command in each job worktree; a failed setup reports `WORKTREE_SETUP_FAILED` with its output.
- Cleanup removes applied or discarded job worktrees and week-old orphans; relaunched jobs reuse their worktree.
- Mechanical jobs can run on a free OpenCode model from a local inventory kept by `/purser:init`, falling back to the next candidate and then your configured coder; the served model drives review family and telemetry.
- The compaction notice repeats every 15 messages with the tokens lost so far while you have not compacted.
- An oversized models.dev cost no longer crashes the catalog, and `/purser:init` lists OpenCode models once.

## 0.1.37 — 2026-09-27

- `/purser:init` asks Codex users how Purser should work (Balanced, Maximum, Manual) and shows the resolved route-to-model pairs before applying; models come from the installed agent's live list, with models.dev facts cached for offline use.
- A complete route table without a Sol model now validates, so Maximum can run every route on the strongest model.
- The reviewer gate uses the coder's verified model family at vendor level, so OpenCode and agy coders can get a different-vendor Reviewer-1.
- An async job whose scope overlaps a running job is queued and starts when that job's lease frees, with a fresh baseline; replies and sync calls are still denied on conflict.
- Plans name each milestone's touched paths and parallel group; an accepted plan runs its milestones in order without asking between them.
- `codex_result` finds a job from its own session whatever the shell's working directory.
- The low-quota denial offers a free background wait until the 5-hour reset.
- The handoff no longer quotes background task notifications as user messages.

## 0.1.36 — 2026-09-27

- `/purser:init` offers OpenCode and Antigravity (agy) as coders when installed, with a model from the agent's own list; the choice persists in `~/.orchestrator/config.toml`.
- agy coder runs are bound to the job directory (agy otherwise writes into its last-used project) and pass the route effort.
## 0.1.35 — 2026-09-27

- The code map runs through the plugin's Python locator, and the operations guide documents it.
- AGENTS.md asks for small targeted edits instead of whole-file rewrites.
- The handoff keeps the last three user messages and the last reply verbatim, refreshed from the transcript at compact.
- OpenCode can be the coder (`coder_agent`); routes, receipts, telemetry and replies follow the agent, and an unknown model family forces FULL review.
- After a consumed Codex result, the Stop hook nudges Claude once to continue instead of asking.
## 0.1.34 — 2026-09-27

- Codex ETA learning is per coding agent; every fifth compact measures estimate error and reports when it improves.
- The unfinished-job reminder skips failed jobs whose waiter already reported them.
- AGENTS.md adds a least-code rule and one test per failure direction.
- A cached per-project code map (Python via ast, TypeScript/TSX via optional tree-sitter) is refreshed before planning and delegation; workers get its path and status only, and a mutating job marks it STALE.
## 0.1.33 — 2026-09-26

- A Codex job that finished its work but whose process did not exit is completed after a 20-second grace instead of hanging as running.

## 0.1.32 — 2026-09-26

- Plan calls accept changes from an overlapping job's leased scope; with two or more active jobs, responses show one task/route/elapsed/state line per job.
- A repair reply on a finished job is attributed to the same task, so other jobs are not reviewed on its files.
- The result line check accepts "%N" or "N%", stops repeating once satisfied, and skips quota when the 5h window has reset.
- The token report splits Codex jobs by project (`--project`).

## 0.1.31 — 2026-09-26

- A job interrupted by a reboot or crash resumes automatically where it stopped (at most twice); failed recovery is reported as uncertain, never as success.
- After a restart the same session is told to fetch or re-await its jobs; cleanup never removes a live job.
- Parallel jobs: each job is reviewed only on its own changes when another job's leased scope changed alongside it.
- The model, effort and quota line after a Codex result is enforced by hooks.

## 0.1.30 — 2026-09-26

- The detached job host starts with a hidden console, so a venv python no longer opens terminal windows.
- A new session reports jobs finished while the app was closed and adopts one only after you agree.
- The compact notice is enforced by hooks and re-shown until it appears.

## 0.1.29 — 2026-09-26

- Codex runs with no console window, so no PowerShell/python windows pop up during async jobs.
- Async jobs can be cancelled and awaited from a new session after a restart; job records survive transient Windows file locks.
- Parallel jobs: implementation calls lease their scope; an overlapping start is denied naming the owner; stale leases expire; `purser_leases.py` lists or releases stuck ones.

## 0.1.28 — 2026-09-25

- Async Codex jobs run in a detached per-job host; a restarted bridge still reports them and returns their result.
- Plan calls: ledger accepts plan records; no contract/validation noise; mutation warning names a possible concurrent writer; /purser:plan hides the waiter command.
- Local releases ship only committed files: release.py exports the release commit to a marketplace outside the repo.

## 0.1.27 — 2026-09-25

- /purser:plan: read-only sol/high plan saved to a file with a 4000-char summary; prompts mentioning planning get a hint to offer it.
- Skill: rare branches moved to references/ (25% smaller); receipts in the turn-ending reply, with time left to the 5h quota reset.
- Skill: finished parallel results presented one at a time; approved multi-part work continues without asking; prompt rules follow GPT-6 guidance.
- Fresh Codex calls cap tool output at 8000 tokens (pilot); scripts/codex_token_report.py compares before/after.
- Ledger no longer flags snapshot paths containing 'token' as secrets; async finalize holds an OS lock and returns LEDGER_RECORD_REJECTED.

## 0.1.26 — 2026-09-25

- Skill: every Codex call runs async; dispatch and result receipts are one bold line with model, effort and quota.
- /purser:optimize: ranked, plain-words report of actionable drains per 500 messages, friendly names, hidden sessions in absolute terms, learning note.
- Init: stores the base Python instead of a venv one, offers Ponytail for UI projects, mentions what Purser learns.
- Reply deny messages name the tool, the disallowed fields, and the recorded task/phase/route mismatch (`a91fc7a`).

## 0.1.25 — 2026-09-25

- **Fix:** codex_start no longer cancels a live job when Claude Code passes the MCP result as plain text (`59da6bc`).

## 0.1.24 — 2026-09-25

- **New:** long Codex work runs async (codex_start / codex_result / codex_cancel + background waiter), so Claude keeps talking to the user while it runs; same enforcement and review as sync.
- **New:** optional ORCH eta=<N>m is recorded beside the actual duration; SessionStart shows this project's per-route median/p90 once 10 samples exist.
- **New:** every prompt carries a one-line reply-language reminder (recorded handoff language; none for English) to stop drift into English (`e38d962`).

## 0.1.23 — 2026-09-25

- **Telemetry:** Codex call events record why the call was made (reason, explicit or inherited) and route changes on a thread (`2ee5fcd`).

## 0.1.22 — 2026-09-25

- **Fix:** runtime state moves from %LOCALAPPDATA%\claude-agent-orchestrator (virtualized per app under the Store Claude app, invisible to other processes) to `PURSER_STATE_DIR` > `CLAUDE_CONFIG_DIR/purser` > `~/.claude/purser`; `purser_migrate_state.py` copies old state once (3b48442) (`3b48442`).

## 0.1.21 — 2026-09-25

- **Fix:** the handoff is no longer injected on `resume` (e.g. after an app restart), where the conversation is still in context; `compact`, `clear` and fresh `startup` unchanged (`3e4a274`).

## 0.1.20 — 2026-09-25

- **Fix:** the handoff records `User language:` and the post-compaction restore tells Claude to reply in it, since the English summary and handoff made Claude switch to English (`01faa15`).

## 0.1.19 — 2026-09-25

- **Fix:** protected paths match case-insensitively on Windows and macOS, and hook checks report edits to already-dirty or untracked files (`b4c1e81`); tests pass in a fresh checkout and no longer leave pytest cache dirs (`1e4d622`, `ccef73e`) (`ccef73e`).

## 0.1.18 — 2026-09-25

- **New:** after each Codex call a systemMessage shows the remaining quota, e.g. '⚓ **Purser:** Codex quota left: 5h 99% (resets 11:57) · 7d 54% (resets Sep 28)' (`310ef85`).

## 0.1.17 — 2026-09-25

- **Fix:** `/purser:optimize` builds the current setup by replaying the session's attachments, so an MCP server the user disabled (removed in `mcp_instructions_delta` / `deferred_tools_delta`) is no longer reported, and a non-initial skill listing adds skills instead of replacing the list (`1997c36`).

## 0.1.16 — 2026-09-24

- **New:** `/purser:optimize` reads the last 30 days of local transcripts (read-only, on demand, ~2 s) and reports: skills, plugins and MCP servers loaded on every request but never used; hidden non-interactive sessions by weighted-token share; duplicate skills; median reply length with a short-reply rule suggestion; repeated shell-command sequences worth one script; and the compaction advice status. `/purser:init` offers it at the end (`b355d5f`).

## 0.1.15 — 2026-09-24

- **Change:** the compaction notice must be the very last text of the turn, after every tool call, so a later tool call or reply cannot bury it (`1c46872`).

## 0.1.14 — 2026-09-24

- **New:** a routed Codex call shows a one-line systemMessage such as 'Purser: route=mechanical -> gpt-6-luna / high', because the call UI shows Claude's input, not the hook's routed model. Desktop only; mobile does not show systemMessage (`898c421`).

## 0.1.13 — 2026-09-24

- **Fix:** the handoff's Purser state block and the task-end trigger take a codex-reply's task from its result bundle when the prompt has no ORCH line, instead of recording task=unknown (`f7004a1`).

## 0.1.12 — 2026-09-24

- **Change:** the `codex-orchestrator` skill caps replies to the user at 5
  plain lines unless they ask for detail (result, one recommendation, at most
  one question; no tables or retelling), because every reply is re-read on
  each later request (`0a9f749`).

## 0.1.11 — 2026-09-24

- **Change:** the compaction notice adds "Nothing is lost." after the handoff
  line (`de0533a`).

## 0.1.10 — 2026-09-24

- **Change:** the compaction suggestion is now a separate closing block after
  a `---` line: context size, the estimated saving per later message
  (`1 - 75K / context`), a `/compact` call to action and the handoff path.
  A question Claude asks in the same reply is also written under Next in the
  handoff so it survives compaction (`33e090a`).

## 0.1.9 — 2026-09-24

- **Change:** the restored handoff tells Claude to skip listed files that are
  already in context, since Claude Code re-attaches recently read files after
  `/compact` (`5010083`).

## 0.1.8 — 2026-09-24

- **New:** compaction advisor step 2. After `/compact` or `/clear`, a
  SessionStart hook injects `purser-handoff.md` (historical reference, capped
  at 6,000 characters; on startup/resume only if written in the last 12
  hours). `/purser:init` creates the handoff template and prints its path
  (`d74e5e8`).

## 0.1.7 — 2026-09-24

- **New:** compaction advisor (step 1 of 2). On a user prompt, when the
  session context is >= 250K tokens and a phase boundary was crossed (>= 20
  min idle, a successful `git commit`, or a reviewed Codex result with a new
  `task=`), the UserPromptSubmit hook asks Claude to fill `purser-handoff.md`
  in Purser's per-project state directory (outside git) and to suggest
  `/compact` to the user. The hook writes a bounded "Purser state" block
  (recent Codex calls, commits, uncommitted count) with no model call, and
  does not repeat until the context grows 60K more. Settings: `[compaction]`
  in `~/.orchestrator/config.toml` (`enabled`, `threshold_tokens`,
  `idle_minutes`, `regrow_tokens`) (`03bc330`). A transcript replay of 65 desktop sessions
  estimated up to ~37% billable-weight saving at 250K.

## 0.1.6 — 2026-09-24

- **Change:** the `codex-orchestrator` skill sends exploration that would take
  Claude three or more read/search turns to one `Explore` agent on Haiku with a
  bounded answer; Claude verifies only the decisive lines. Each Claude turn
  re-reads the whole context, so exploration in a small side context is cheaper.

## 0.1.5 — 2026-09-24

- **Change:** the `codex-orchestrator` skill now sets the language rule: only
  replies to the user use the user's language; worker prompts, required
  reports, commits, state and repository documents are English, because
  non-English text costs more tokens and returns in every bundle.

## 0.1.4 — 2026-09-24

- **Fix:** hooks now read and write their JSON as UTF-8 on Windows instead of
  the console code page. Before, a bundle containing a character outside that
  code page (a failing Vitest tail's `❯`) crashed the PostToolUse hook, and
  Claude silently received the raw Codex result without the
  `HOOK_CHECK_FAILED` bundle; non-ASCII prompt text was also garbled, and could
  crash the Codex bridge with a lone surrogate. The Python locator now sets
  `PYTHONUTF8=1` for every hook and the bridge (`f1575e6`).

## 0.1.3 — 2026-09-24

- **Fix:** a `codex-reply` now resolves to the project its thread was created
  in. Before, it used the Claude session's current directory, so after the
  session moved into another git repository (for example a nested clone) the
  reply was denied `NO_LIVE_THREAD`. A recorded project that is no longer a git
  work tree is denied instead of falling back (`688f680`).
- **Change:** `route=normal`, `consequential` and `escalated` (gpt-6-sol) now
  require `basis=` naming the design choice left to the worker; without it the
  call is denied with `SOL_ROUTE_NEEDS_BASIS` and pointed to `route=subtle`
  (`363b1f5`).

## 0.1.2 — 2026-09-24

- **New:** low-quota warning. A fresh `codex` call is denied once with
  `LOW_CODEX_QUOTA` when a live 5h or 7d Codex window is at >= 90% used; resend
  with `reason=user_override` to proceed. Replies are never checked and a
  missing reading never blocks (`5adb4df`).

## 0.1.1 — 2026-09-24

- **Fix:** Reviewer-1 failures are now diagnosable: telemetry carries
  `reviewer1_error_code` and elapsed time, and agy's stderr tail is saved and
  linked from the finding. A non-zero agy exit under 60 s is retried once
  within the same timeout (`409fcaf`).

## 0.1.0 — 2026-09-23

- First plugin release (P6a-P6d): hooks, skill, Codex exec bridge,
  `/purser:check`, `/purser:init`, Python locator, cutover script.
