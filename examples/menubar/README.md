# localci in the menu bar

A [SwiftBar](https://swiftbar.app) (or xbar) plugin that shows what localci runs without opening the dashboard.

- **The title** says how many runs are running and the worst state: `idle`, `2 running`, or `failed` (red) when the
  newest run of some suite or check failed. A later passing run of that suite clears it.
- **The menu** lists the runs in progress with how long they have run and how much time is left, the last five
  results, and a link to the dashboard. A failure has a submenu: open the run, or retry it.
- When localci is not running the title reads `localci off`.

## Install

1. Install SwiftBar and choose a plugin folder.
2. Copy `localci.10s.sh` into that folder and make it executable: `chmod +x localci.10s.sh`.
3. The menu refreshes every 10 seconds (the `10s` in the file name).

The plugin calls `localci menubar`, which prints the menu from the local API. To use another port for the
dashboard link, set `LOCALCI_SERVER_PORT` (or pass `--dashboard http://127.0.0.1:<port>` in the script).

## Notifications

`localci serve` also shows a desktop notification when a run you started by hand (`localci run`, or a retry) ends.
Choose which outcomes with `notify.on` in the config (`failed`, `passed`; both by default).

- macOS uses `terminal-notifier` when it is installed (clicking the notification opens the run in the
  dashboard) and `osascript` otherwise.
- Linux uses `notify-send`.
