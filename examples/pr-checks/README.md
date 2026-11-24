# Pull request checks

`localci.yaml` here is a complete configuration: no suite, only the workflow checks of one repository.

```bash
cp localci.yaml ~/.config/localci/config.yaml    # edit the repository name first
localci doctor                                   # what this machine can run
localci serve                                    # watches pull requests; dashboard at http://127.0.0.1:7357
localci plan                                     # from a checkout: which checks a pull request would run, and why others are skipped
localci run --checks --wait                      # run this checkout's checks now
```

Each job appears on the pull request as a `localci/<workflow>/<job>` status, and `localci/checks` summarises them.
