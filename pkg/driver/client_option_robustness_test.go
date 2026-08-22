package driver

import "testing"

func TestNewSkipsNilOptionsAndClients(t *testing.T) {
	client := New(nil, WithClient(nil), WithRestyClient(nil))
	if client == nil || client.Client == nil {
		t.Fatalf("nil option/client injection destroyed default client: %#v", client)
	}
	if request := client.NewRequest(); request == nil {
		t.Fatal("client with nil options cannot create a request")
	}
}

func TestSetHttpClientNilIsNoOp(t *testing.T) {
	client := New()
	original := client.Client
	if got := client.SetHttpClient(nil); got != client {
		t.Fatalf("SetHttpClient(nil) returned %#v, want original client %#v", got, client)
	}
	if client.Client != original {
		t.Fatal("SetHttpClient(nil) replaced the existing resty client")
	}
}

func TestImportCredentialNilIsNoOp(t *testing.T) {
	client := New()
	if got := client.ImportCredential(nil); got != client {
		t.Fatalf("ImportCredential(nil) returned %#v, want original client %#v", got, client)
	}
}
