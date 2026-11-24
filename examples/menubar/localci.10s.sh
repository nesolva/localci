#!/bin/bash
# <xbar.title>localci</xbar.title>
# <xbar.desc>What localci is running and how the last runs ended.</xbar.desc>
# <xbar.dependencies>localci</xbar.dependencies>
# <swiftbar.hideAbout>true</swiftbar.hideAbout>
# <swiftbar.hideRunInTerminal>true</swiftbar.hideRunInTerminal>
#
# Drop this file in your SwiftBar (or xbar) plugin folder and make it executable. The "10s" in the name is how
# often the menu refreshes. Set LOCALCI_BIN if localci is somewhere unusual, and LOCALCI_SERVER_PORT if you changed
# `server.port`.

# Menu bar apps start with a short PATH: add the usual install locations.
export PATH="$PATH:/opt/homebrew/bin:/usr/local/bin:$HOME/go/bin:$HOME/.local/bin"

bin="${LOCALCI_BIN:-$(command -v localci)}"
if [ -z "$bin" ]; then
  echo "localci missing | sfimage=questionmark.circle"
  echo "---"
  echo "localci is not installed or not on the PATH"
  exit 0
fi
exec "$bin" menubar --bin "$bin"
