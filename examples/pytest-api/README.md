# pytest-api: an HTTP API suite

A small orders API and its mocked payments backend (`orders/`, `fakes/`, standard library only), tested with pytest
through [`localci-pytest`](../../packages/pytest). localci starts both as the suite's `stack`, on ports of the shard's
own, and runs `tests/` against them in two shards.

Python 3.9 or newer.

```bash
git clone https://github.com/nesolva/localci
cp -R localci/examples/pytest-api ~/acme-orders && cd ~/acme-orders
git init -q -b main && git remote add origin https://github.com/acme/orders.git   # the name localci.yaml uses
git add -A && git commit -qm sample

# localci clones repositories from GitHub. Point the sample's made-up address at this folder instead:
git config --global url."$PWD".insteadOf https://github.com/acme/orders.git

mkdir -p ~/.config/localci && cp localci.yaml ~/.config/localci/config.yaml
localci serve &                      # dashboard at http://127.0.0.1:7357
localci run --suite pytest-api --wait
```

The last line says how it ended: `#1 passed 12/12 · 6s`. In the dashboard, a test's page lists its steps: the setup of
its fixtures, the `with localci_pytest.step(...)` blocks and the teardown.

To see a failure report, stop the server and start it again with the bug switched on: `ACME_BREAK=1 localci serve &`.
The bulk discount then starts at two items instead of ten, and

```bash
localci run --suite pytest-api --wait
localci show 2                       # step: check the total · expected: 2700 · actual: 2430
```

Things the sample shows:

- `requirements.txt` installs the plugin from this repository (a `pip install` of the package once it is published);
  `prepare` keeps the virtualenv between runs.
- `pytest.ini` sits at the repository's root, so `list` and the run agree on every test's `file` and id.
- The tests read `ORDERS_URL` and `PAYMENTS_URL` inside fixtures, because localci lists the suite without the
  stack running and without the suite's `env`.

When you are done: stop `localci serve` and remove the redirect with
`git config --global --unset url."<the folder you used>".insteadOf`.
