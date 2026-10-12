package runtimes

import (
	"strings"
	"testing"
)

func TestNodeRangeBoundaries(t *testing.T) {
	// Bounds follow npm/node-semver's strict stable range expansion. In
	// particular, partial > and <= are not ordinary zero-filled comparisons.
	tests := []struct {
		rangeText string
		allowed   []string
		rejected  []string
	}{
		{"", []string{"0.0.0", "24.20.0"}, nil},
		{"*", []string{"0.0.0", "24.20.0"}, nil},
		{"x.x", []string{"0.0.0", "24.20.0"}, nil},
		{"1.2.3", []string{"1.2.3", "v1.2.3"}, []string{"1.2.2", "1.2.4"}},
		{"=v1.2.3", []string{"1.2.3"}, []string{"1.2.4"}},
		{"1", []string{"1.0.0", "1.99.99"}, []string{"0.99.99", "2.0.0"}},
		{"1.2", []string{"1.2.0", "1.2.99"}, []string{"1.1.99", "1.3.0"}},
		{"1.X.*", []string{"1.0.0", "1.99.99"}, []string{"2.0.0"}},
		{"1.2.x", []string{"1.2.0", "1.2.99"}, []string{"1.3.0"}},
		{">1", []string{"2.0.0", "24.20.0"}, []string{"1.0.1", "1.99.99"}},
		{">1.2", []string{"1.3.0", "2.0.0"}, []string{"1.2.1", "1.2.99"}},
		{">1.2.3", []string{"1.2.4", "2.0.0"}, []string{"1.2.3"}},
		{">=1.2", []string{"1.2.0", "1.3.0"}, []string{"1.1.99"}},
		{"<=1", []string{"0.0.0", "1.99.99"}, []string{"2.0.0"}},
		{"<=1.2", []string{"1.1.99", "1.2.99"}, []string{"1.3.0"}},
		{"<=1.2.3", []string{"1.2.2", "1.2.3"}, []string{"1.2.4"}},
		{"<1.2", []string{"0.0.0", "1.1.99"}, []string{"1.2.0", "1.2.1"}},
		{">= 1.2.3\t< 1.3", []string{"1.2.3", "1.2.99"}, []string{"1.2.2", "1.3.0"}},
		{">*", nil, []string{"0.0.0", "24.20.0"}},
		{"<x", nil, []string{"0.0.0", "24.20.0"}},
		{"<=*", []string{"0.0.0", "24.20.0"}, nil},
		{">=x", []string{"0.0.0", "24.20.0"}, nil},
		{"^1.2.3", []string{"1.2.3", "1.99.99"}, []string{"1.2.2", "2.0.0"}},
		{"^0.2.3", []string{"0.2.3", "0.2.99"}, []string{"0.2.2", "0.3.0"}},
		{"^0.0.3", []string{"0.0.3"}, []string{"0.0.2", "0.0.4", "0.1.0"}},
		{"^0.0.0", []string{"0.0.0"}, []string{"0.0.1"}},
		{"^0.0", []string{"0.0.0", "0.0.99"}, []string{"0.1.0"}},
		{"^0.0.x", []string{"0.0.0", "0.0.99"}, []string{"0.1.0"}},
		{"^0", []string{"0.0.0", "0.99.99"}, []string{"1.0.0"}},
		{"^1.2", []string{"1.2.0", "1.99.99"}, []string{"1.1.99", "2.0.0"}},
		{"~1.2.3", []string{"1.2.3", "1.2.99"}, []string{"1.2.2", "1.3.0"}},
		{"~1.2", []string{"1.2.0", "1.2.99"}, []string{"1.1.99", "1.3.0"}},
		{"~>1.2", []string{"1.2.0", "1.2.99"}, []string{"1.3.0"}},
		{"~1", []string{"1.0.0", "1.99.99"}, []string{"2.0.0"}},
		{"~0.0.3", []string{"0.0.3", "0.0.99"}, []string{"0.0.2", "0.1.0"}},
		{"1.2.3 - 2.3.4", []string{"1.2.3", "2.3.4"}, []string{"1.2.2", "2.3.5"}},
		{"1.2 - 2.3", []string{"1.2.0", "2.3.99"}, []string{"1.1.99", "2.4.0"}},
		{"1.2.3 - 2", []string{"1.2.3", "2.99.99"}, []string{"1.2.2", "3.0.0"}},
		{"* - 2", []string{"0.0.0", "2.99.99"}, []string{"3.0.0"}},
		{"2 - *", []string{"2.0.0", "24.20.0"}, []string{"1.99.99"}},
		{"^20.19.0 || >=22.12.0", []string{"20.19.0", "20.99.99", "22.12.0", "24.20.0"}, []string{"20.18.99", "21.0.0", "22.11.99"}},
		{">=16 || 14 >=14.17", []string{"14.17.0", "14.99.99", "16.0.0"}, []string{"14.16.99", "15.0.0"}},
		{"1 ||", []string{"0.0.0", "24.20.0"}, nil},
	}
	for _, test := range tests {
		t.Run(test.rangeText, func(t *testing.T) {
			for _, version := range test.allowed {
				if err := CheckNode(version, []string{test.rangeText}); err != nil {
					t.Errorf("%s should satisfy %q: %v", version, test.rangeText, err)
				}
			}
			for _, version := range test.rejected {
				err := CheckNode(version, []string{test.rangeText})
				if err == nil || !strings.Contains(err.Error(), "does not satisfy") {
					t.Errorf("%s should fail compatibility with %q: %v", version, test.rangeText, err)
				}
			}
		})
	}
}

func TestNodeRejectsUnsupportedVersionsAndSyntax(t *testing.T) {
	for _, version := range []string{"", "24", "24.20", "24.20.x", "24.20.0.1", "v24.20.0-rc.1", "24.20.0+build", "024.20.0", "24.020.0", "=24.20.0", "vv24.20.0", "9007199254740992.0.0"} {
		if err := CheckNode(version, nil); err == nil {
			t.Errorf("unsupported runtime %q accepted", version)
		}
	}
	for _, constraint := range []string{"latest", "lts/*", "node >=20", ">=20, <25", "!=22", "<>22", ">>20", ">=20 | <25", ">=20 && <25", "(>=20)", ">=20.0.0-rc.1", ">=20.0.0+build", "1.x.2", "x.2", "1..2", "01.2.3", ">=", "^", "1-2", "1 -", "1 - 2 <3", "<3 1 - 2", "9007199254740992", "9007199254740991", strings.Repeat(" ", nodeMaxRangeBytes+1)} {
		err := CheckNode("24.20.0", []string{constraint})
		if err == nil || !strings.Contains(err.Error(), "unsupported Node engine constraint") {
			t.Errorf("unsupported constraint %q accepted or misreported: %v", constraint, err)
		}
	}
	for _, constraints := range [][]string{{">=20 || broken"}, {"broken || >=20"}, {">=20", "broken"}, {"<1", "broken"}, {"* || 20.0.0-rc.1"}} {
		err := CheckNode("24.20.0", constraints)
		if err == nil || !strings.Contains(err.Error(), "unsupported Node engine constraint") {
			t.Errorf("unsupported range hidden by a branch/declaration: %v: %v", constraints, err)
		}
	}
}

func TestNodeRequiresEveryEngineDeclaration(t *testing.T) {
	if err := CheckNode("24.20.0", []string{">=20", "<24"}); err == nil {
		t.Fatal("a matching engine declaration hid an incompatible declaration")
	}
	if err := CheckNode("24.20.0", []string{">=20", "<25"}); err != nil {
		t.Fatal(err)
	}
}

func TestNodeFrozenZeframEngines(t *testing.T) {
	// Unique selected linux/x64/glibc package engines from Zefram commit
	// c8edd7771a0be62b73014874f000e013fedd9d29 (112 package placements).
	constraints := []string{
		"6.* || 8.* || >= 10.*", ">= 0.4", ">= 12.0.0", ">= 20", ">= 6",
		">=0.10.0", ">=0.8", ">=10", ">=10.0.0", ">=10.13.0", ">=12",
		">=12.0.0", ">=16 || 14 >=14.17", ">=18", ">=18.20 <19 || >=20.10",
		">=20", ">=22", ">=22.12.0", ">=22.13.0", ">=6", ">=6.0.0", ">=8", ">=8.6",
		"^10 || ^12 || >=14", "^10 || ^12 || ^13.7 || ^14 || >=15.0.1",
		"^12.17.0 || ^14.13 || >=16.0.0", "^12.20.0 || ^14.13.1 || >=16.0.0",
		"^20.19.0 || >=22.12.0", "^20.19.0 || ^22.12.0 || >=23",
	}
	if err := CheckNode("v24.20.0", constraints); err != nil {
		t.Fatal(err)
	}
	if err := CheckNode("22.12.0", constraints); err == nil {
		t.Fatal("22.12.0 must fail the full graph's >=22.13.0 requirement")
	}
	if err := CheckNode("20.19.0", constraints); err == nil {
		t.Fatal("20.19.0 must fail the full graph's >=22 requirements")
	}
}
