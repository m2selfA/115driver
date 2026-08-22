package cmd

import (
	"errors"
	"reflect"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
)

type fakeAppVersionClient struct {
	versions []driver.AppVersion
	err      error
}

func (f *fakeAppVersionClient) GetAppVersion() ([]driver.AppVersion, error) {
	if f.err != nil {
		return nil, f.err
	}
	return append([]driver.AppVersion(nil), f.versions...), nil
}

func TestLoadAppVersionsPreservesDriverOrderAndFields(t *testing.T) {
	client := &fakeAppVersionClient{versions: []driver.AppVersion{
		{AppName: "android", Version: "35.2.0"},
		{AppName: "win", Version: "32.1.0.0"},
		{AppName: "unknown", Version: ""},
	}}
	got, err := loadAppVersions(client)
	if err != nil {
		t.Fatal(err)
	}
	want := []appVersionResult{
		{App: "android", Version: "35.2.0"},
		{App: "win", Version: "32.1.0.0"},
		{App: "unknown", Version: ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loadAppVersions() = %#v, want %#v", got, want)
	}
}

func TestLoadAppVersionsPropagatesDriverError(t *testing.T) {
	want := errors.New("version service unavailable")
	_, err := loadAppVersions(&fakeAppVersionClient{err: want})
	if !errors.Is(err, want) {
		t.Fatalf("loadAppVersions error = %v, want %v", err, want)
	}
}

func TestAppVersionCommandHasStrictNoArgsContract(t *testing.T) {
	if appVersionCmd.Args == nil {
		t.Fatal("app-version command has no Args validator")
	}
	if err := appVersionCmd.Args(appVersionCmd, []string{"unexpected"}); err == nil {
		t.Fatal("app-version accepted a positional argument")
	}
}

func TestAppVersionCommandSkipsAccountAuthentication(t *testing.T) {
	if !commandSkipsAuthentication(appVersionCmd) {
		t.Fatal("app-version unexpectedly requires account authentication")
	}
}

func TestAppVersionCommandClientWorksWithoutAuthenticatedGlobalClient(t *testing.T) {
	oldClient, oldDebug := client, debugMode
	client = nil
	debugMode = false
	t.Cleanup(func() {
		client, debugMode = oldClient, oldDebug
	})
	if got := appVersionCommandClient(); got == nil {
		t.Fatal("app-version standalone client is nil")
	}
}
