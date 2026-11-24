package affected

import "testing"

func TestExplainSaysWhyEachTestOrWhyAll(t *testing.T) {
	in := sample()
	in.Changed = []string{shop + ":public/pay.js", shop + ":README.md"}
	got := Explain(Select(in))
	want := `changed: 2 files
  acme/shop:public/pay.js
  acme/shop:README.md
1 of 5 tests affected
  tests/checkout.spec.ts checkout  because acme/shop:public/pay.js
`
	if got != want {
		t.Fatalf("explain =\n%s\nwant\n%s", got, want)
	}

	in.Changed = []string{shop + ":package-lock.json"}
	got = Explain(Select(in))
	want = `changed: 1 file
  acme/shop:package-lock.json
all 5 tests: acme/shop:package-lock.json matches always_all (package-lock.json)
`
	if got != want {
		t.Fatalf("explain =\n%s\nwant\n%s", got, want)
	}
}
