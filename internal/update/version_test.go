package update

import "testing"

func TestParseVersion(t *testing.T) {
	cases := []struct {
		raw     string
		major   int
		minor   int
		patch   int
		pre     []string
		wantErr bool
	}{
		{raw: "v0.1.0", major: 0, minor: 1, patch: 0},
		{raw: "1", major: 1},
		{raw: "1.2", major: 1, minor: 2},
		{raw: "v1.2.3", major: 1, minor: 2, patch: 3},
		{raw: "1.2.3-rc.1", major: 1, minor: 2, patch: 3, pre: []string{"rc", "1"}},
		{raw: "1.2.3+build.5", major: 1, minor: 2, patch: 3},
		// git describe 的后缀是一整个 prerelease 标识符：3-gabc123-dirty。
		{raw: "0.1.0-3-gabc123-dirty", major: 0, minor: 1, patch: 0, pre: []string{"3-gabc123-dirty"}},
		{raw: "dev", wantErr: true},
		{raw: "", wantErr: true},
		{raw: "1.2.3.4", wantErr: true},
		{raw: "1.2.x", wantErr: true},
		{raw: "1.+2.3", wantErr: true},
	}
	for _, testCase := range cases {
		version, err := ParseVersion(testCase.raw)
		if testCase.wantErr {
			if err == nil {
				t.Errorf("ParseVersion(%q) 应该失败，得到 %+v", testCase.raw, version)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseVersion(%q) 失败：%v", testCase.raw, err)
			continue
		}
		if version.Major != testCase.major || version.Minor != testCase.minor || version.Patch != testCase.patch {
			t.Errorf("ParseVersion(%q) = %d.%d.%d，期望 %d.%d.%d", testCase.raw,
				version.Major, version.Minor, version.Patch, testCase.major, testCase.minor, testCase.patch)
		}
		if len(version.Pre) != len(testCase.pre) {
			t.Errorf("ParseVersion(%q) prerelease = %v，期望 %v", testCase.raw, version.Pre, testCase.pre)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		left  string
		right string
		want  int
	}{
		{left: "0.1.0", right: "0.2.0", want: -1},
		{left: "1.0.0", right: "0.9.9", want: 1},
		{left: "1.0.0", right: "1.0.0", want: 0},
		{left: "1", right: "1.0.0", want: 0},
		{left: "0.1.0-rc.1", right: "0.1.0", want: -1},
		{left: "0.1.0", right: "0.1.0-rc.1", want: 1},
		{left: "0.1.0-rc.1", right: "0.1.0-rc.2", want: -1},
		{left: "0.1.0-alpha", right: "0.1.0-beta", want: -1},
		{left: "1.0.0-rc.1", right: "1.0.0-rc.1", want: 0},
		{left: "v0.1.0", right: "0.1.0", want: 0},
	}
	for _, testCase := range cases {
		left, err := ParseVersion(testCase.left)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", testCase.left, err)
		}
		right, err := ParseVersion(testCase.right)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", testCase.right, err)
		}
		if got := left.Compare(right); got != testCase.want {
			t.Errorf("%s.Compare(%s) = %d，期望 %d", testCase.left, testCase.right, got, testCase.want)
		}
	}
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		current string
		latest  string
		want    bool
	}{
		{current: "0.1.0", latest: "v0.1.1", want: true},
		{current: "0.1.0", latest: "0.1.0", want: false},
		{current: "0.2.0", latest: "0.1.0", want: false},
		{current: "dev", latest: "0.1.0", want: true},
		{current: "0.1.0-3-gabc-dirty", latest: "0.1.0", want: true},
		{current: "0.1.0", latest: "not-a-version", want: false},
	}
	for _, testCase := range cases {
		if got := IsNewer(testCase.current, testCase.latest); got != testCase.want {
			t.Errorf("IsNewer(%q, %q) = %v，期望 %v", testCase.current, testCase.latest, got, testCase.want)
		}
	}
}
