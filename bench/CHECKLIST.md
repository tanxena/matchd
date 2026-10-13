# Benchmark pre-flight checklist

For `make bench-full` (about **75 min**, High performance plan) and `make bench-balanced` (about **10 min**, Balanced plan): about 90 min in all, plus switching plans in between. The why behind each step is in [decision 021](../docs/decisions/021-benchmark-harness.md) and the [learning doc](../docs/learn/09-benchmarks.md).

Run every `make` from **PowerShell or cmd, not Git Bash** (decision 010 §13). `make` needs `C:\msys64\usr\bin` and `C:\msys64\ucrt64\bin` on `PATH`.

## 0. Once: check the harness (2 minutes)

```powershell
make bench-smoke
```

It runs every configuration once, for 2 s warmup + 5 s, under whatever plan is active, then generates `bin\bench-smoke\report.md` from the results. Expect:
- 10 lines like `[ 5/10] 05-mix-resting-r1   20007/s  core p50 …  e2e p50 …  cpu eng … gen … other …`;
- `done in about 1m20s: 10 runs, N disturbed, 0 failed`;
- **0 failed.** "Disturbed" is expected here if anything else is running: the line names what (e.g. `chrome.exe 1.63`), which is the point of the check;
- a report that starts with "Smoke test of the harness, not benchmark results".

If any run **failed**, stop: its file in `bin\bench-smoke\` has a `failed` field with the error.

## 1. Power (before `bench-full`)

1. **Plug in the charger,** and leave it in for the whole run. A run on battery, or one where the power source changed, is marked disturbed.
2. **The High performance plan.** It isn't listed on this machine (only "Balanced" and "My Custom Plan 1" are), so restore it once:
   ```powershell
   powercfg /duplicatescheme 8c5e7fda-e8bf-4a96-9a85-a6e23a8c635c
   ```
   This prints `Power Scheme GUID: <new-guid>  (High performance)`. Activate it:
   ```powershell
   powercfg /setactive <new-guid>
   powercfg /getactivescheme
   ```
   `benchrun` checks the plan's *personality* (what kind of plan it is), not its GUID, so the copy's new GUID is fine. The hidden original reads as "high performance" on this machine, and a copy should keep that setting. `benchrun` refuses to start `full` unless the active plan's personality is "high performance". If it refuses with the copy active, tell me rather than using `-force`.
3. **Power mode.** The probe found Settings > System > Power > *Power mode* set to **"Best power efficiency"** on this machine. Under High performance the slider may be unavailable, but the overlay is still recorded in every result file. Before switching plans, set Power mode to **"Best performance"** so the recorded overlay doesn't contradict the plan.

## 2. Nothing else running

`benchrun` measures this and marks a run disturbed if other processes average over **0.35 logical CPUs** during its 60 s window. Things that tripped it in the smoke runs:

| close | why |
|---|---|
| **Chrome / Edge** (all windows, including background apps: check the tray) | a tab playing media used 1–2 CPUs |
| audio and video (anything that makes `audiodg.exe` busy) | |
| **Claude Code**: leave the session idle (no prompt running) or close it | an active session used about 0.1 CPU |
| IDEs and editors with indexers, OneDrive or Dropbox sync, Teams, Slack, Discord | |
| Windows Update: Settings > Windows Update > *Pause updates* for a week | |
| Microsoft Defender's scheduled scan: don't start a run just after boot or login | `MsMpEng.exe` appeared in a smoke run |

Also:
- **Don't use the machine during the run.** Moving windows and typing make `dwm.exe` redraw. Let the screen stay on (Settings > System > Power > Screen and sleep: *never*, plugged in) so it doesn't sleep. A locked screen is fine.
- **One terminal,** showing benchrun's output. Its scrolling costs a little window-manager time, measured as part of "other".

## 3. Start the full suite

```powershell
make bench-full
```

- It prints `power: AC power, "High performance" plan (high performance)…` and `suite full: 45 runs, about 1h14m`.
- It refuses to start if a check fails: on battery, the wrong plan, ports 19000/19100 in use, under 5 GB free in `%TEMP%`, or `bench\results\full` not empty. The message says which.
- Runs go in this order, 5 rounds of 9: clock probe, then fsync `always`, `group:1ms`, `never`, then mix `resting`, `crossing`, `cancel-heavy`, then snapshots on, off.
- **What the processes are,** if you look in Task Manager: `engine.exe` pinned to CPUs 0–3, `loadgen.exe` to 4–7, `benchrun.exe` waiting. To check the pinning by hand: Task Manager > Details > right-click `engine.exe` > *Set affinity*, which should show 0–3. Every result file also records the mask read back from the OS.
- **To stop:** Ctrl-C. The current run is abandoned, and the finished runs' files stay. A suite can't be resumed: delete `bench\results\full` and start again (interleaving needs the whole suite in one go).

## 4. How to tell a run was disturbed

On the console, each line ends with `DISTURBED: <reasons>` if any check failed:

| reason | meaning |
|---|---|
| `other processes used X CPUs … : chrome.exe 0.80, …` | something else was running; it names the top 5 |
| `on battery power` / `power plan or source changed` | the charger or plan changed during the run |
| `load generator fell behind its schedule: send lag p99 …` | the load generator couldn't keep to its timetable (usually the same cause as "other processes", or the machine stalled) |
| `answered X/s of the 20000/s target` | the rate wasn't sustained |
| `measured window lasted …` | the machine stalled (sleep, a long freeze) |
| `cap cancels` | the book outgrew its bound; the mix was distorted |
| `affinity reads back as …` | pinning didn't take |

Every result file holds the same in its `disturbed` list, plus the raw CPU figures (`load.cpu`: `other`, `top_other`, `interrupts`, `machine_busy`).

**If any run is disturbed:** the report won't use it, and won't generate at all until you decide. Either:
- re-run the whole suite: delete `bench\results\full`, fix the cause, `make bench-full` (cleanest);
- or accept fewer runs for that row: `bin\report -results bench\results -allow-disturbed -readme README.md`. That row then shows "4 of 5†", and the disturbed run is listed under the table with its reason. PRD 7.5 asks for at least 5, so the report refuses a row with fewer than 5 counted runs unless you also pass `-min-runs 4`.

## 5. The Balanced row

After `bench-full` finishes:

```powershell
powercfg /setactive 381b4222-f694-41f0-9685-ff5bb260df2e
```

That's Balanced. Set the Power mode slider to what you want the row to represent, probably Windows' default, **"Balanced"**. It's recorded and shown in the row's label. Then:

```powershell
make bench-balanced
```

About 10 minutes: 5 rounds of (clock probe, resting mix). The same checks and refusals apply, with "balanced" as the required personality.

## 6. Afterwards

```powershell
make bench-report
git add bench/results README.md
```

`bench-report` regenerates the README's benchmark section from every file in `bench\results` (`bench/report`), and refuses if anything is disturbed (step 4). Commit the results and the README together; I'll write the rest of the README around them.

To restore your usual plan: `powercfg /setactive <guid>` (`powercfg /list` shows them).

## Reproducing one run by hand

`make bench-build`, then `bin\benchrun -suite full -dry-run` prints every run's two commands as cmd.exe lines with `start /affinity`. Run the engine line first, then the load-generator line with `-engine-pid` set to the engine's PID (Task Manager > Details), or left at 0 to skip measuring the engine's CPU.
