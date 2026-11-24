package affected

import (
	"slices"
	"testing"
)

func parse(t *testing.T, format, data, root string, tracked ...string) []string {
	t.Helper()
	got, _, err := ParseDump(format, []byte(data), root, func() []string { return tracked })
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestFilesDumpMapsToRepoFiles(t *testing.T) {
	got := parse(t, "files", `# written by the api
src/orders.js
./src/./pay.js
/work/api/src/abs.js
/elsewhere/other.js
../outside.js
node_modules/dep/index.js
src/orders.js
`, "/work/api")
	want := []string{"src/abs.js", "src/orders.js", "src/pay.js"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestLcovKeepsOnlyFilesThatRan(t *testing.T) {
	got := parse(t, "lcov", `TN:
SF:/work/api/src/orders.js
DA:1,0
DA:2,4
end_of_record
SF:/work/api/src/loaded-only.js
DA:1,0
DA:2,0
end_of_record
SF:src/pay.js
DA:7,1
end_of_record
`, "/work/api")
	want := []string{"src/orders.js", "src/pay.js"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v: a file with every count at zero was only loaded", got, want)
	}
}

func TestJacocoReportMapsPackageAndNameToTrackedFiles(t *testing.T) {
	got := parse(t, "jacoco-xml", `<?xml version="1.0"?>
<report name="api">
 <package name="com/acme/orders">
  <sourcefile name="OrderService.java"><counter type="INSTRUCTION" missed="0" covered="12"/></sourcefile>
  <sourcefile name="Unused.java"><counter type="INSTRUCTION" missed="9" covered="0"/></sourcefile>
 </package>
</report>`, "/work/api",
		"src/main/java/com/acme/orders/OrderService.java", "src/main/java/com/acme/orders/Unused.java")
	want := []string{"src/main/java/com/acme/orders/OrderService.java"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestV8CoverageNeedsAFunctionThatRan(t *testing.T) {
	got := parse(t, "v8", `{"result":[
 {"url":"file:///work/api/src/pay.js","functions":[{"functionName":"","ranges":[{"count":1}]},{"functionName":"charge","ranges":[{"count":2}]}]},
 {"url":"file:///work/api/src/loaded.js","functions":[{"functionName":"","ranges":[{"count":1}]},{"functionName":"unused","ranges":[{"count":0}]}]},
 {"url":"node:internal/x.js","functions":[{"functionName":"f","ranges":[{"count":1}]}]}]}`, "/work/api")
	if !slices.Equal(got, []string{"src/pay.js"}) {
		t.Fatalf("got %v", got)
	}
}

func TestUnknownFormatIsAnError(t *testing.T) {
	if _, _, err := ParseDump("gcov", nil, "/x", nil); err == nil {
		t.Fatal("want an error naming the formats")
	}
}
