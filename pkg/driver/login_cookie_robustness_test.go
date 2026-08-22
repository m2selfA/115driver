package driver

import "testing"

func TestImportCookiesSkipsEmptyDomainsWithoutPanicking(t *testing.T) {
	client := New()
	client.ImportCookies(map[string]string{"UID": "1"}, "", "   ")
}

func TestCredentialFromCookieAllowsEqualsInsideValues(t *testing.T) {
	var credential Credential
	if err := credential.FromCookie("UID=1; CID=2; SEID=abc==; KID=k=v"); err != nil {
		t.Fatal(err)
	}
	if credential.UID != "1" || credential.CID != "2" || credential.SEID != "abc==" || credential.KID != "k=v" {
		t.Fatalf("unexpected parsed credential: %#v", credential)
	}
}

func TestCredentialFromCookieRejectsEmptyKey(t *testing.T) {
	var credential Credential
	if err := credential.FromCookie("UID=1; CID=2; SEID=abc; =value"); err == nil {
		t.Fatal("empty cookie key unexpectedly accepted")
	}
}
