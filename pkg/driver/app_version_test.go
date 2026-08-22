package driver

import (
	"reflect"
	"testing"
)

func TestVersionsGetAppVersionsHasExactLengthAndStableOrder(t *testing.T) {
	versions := Versions{
		"win":     {"version_code": "32.1.0.0"},
		"android": {"version_code": "35.2.0"},
	}
	got := versions.GetAppVersions()
	want := []AppVersion{
		{AppName: "android", Version: "35.2.0"},
		{AppName: "win", Version: "32.1.0.0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GetAppVersions() = %#v, want %#v", got, want)
	}
}

func TestVersionsGetAppVersionsDoesNotPanicOnMalformedVersionCode(t *testing.T) {
	versions := Versions{
		"missing": {},
		"numeric": {"version_code": 123},
		"valid":   {"version_code": "1.2.3"},
	}
	got := versions.GetAppVersions()
	want := []AppVersion{
		{AppName: "missing", Version: ""},
		{AppName: "numeric", Version: ""},
		{AppName: "valid", Version: "1.2.3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GetAppVersions() = %#v, want %#v", got, want)
	}
}
