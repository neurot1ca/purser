# Dispatch and result line examples

After each call, report bundle model and effort plus `codex_quota` 5h/7d used percentages and reset times when present; show the `5h` reset as time remaining (e.g. "resets in 1h 20m") and the `7d` reset as time remaining (e.g. "resets in 3d 2h", or "resets in 6h 37m" when under a day). Format the completion receipt as one line `> ⚓ **Codex done: ...**` in the user's language; explain below in plain text.

```text
> ⚓ **Codex: luna/high · operation · fresh · ~6 min (stall after 15 min)**
```

```text
> ⚓ **Codex done: luna/high · 2.4 min · quota 5h 59% (resets in 1h 20m), 7d 56%**
```
