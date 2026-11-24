# Security policy

## Reporting a vulnerability

Please report it privately, not in a public issue. Open the repository's **Security** tab and choose **Report a
vulnerability** (GitHub private vulnerability reporting). Say what you found, how to reproduce it, and which version
(`localci version`) you ran.

We read every report, answer as soon as we can, and tell you when a fix is released. If you would like credit, say so
and how you want to be named.

## Supported versions

Fixes go into the latest release. Older releases do not get backports.

## Scope

localci runs your own tests, apps and workflows on your own machines. [The security model](docs/security.md)
says what runs where and with whose rights. A report that needs the attacker to already run code as your user on
your machine is out of scope; one that lets a pull request, a web page or another machine do something the model says
it cannot is in scope.
