package driver

import (
	"errors"
	"net/http"
	"testing"
)

func TestMutationInputsRejectBlankIDsBeforeNetwork(t *testing.T) {
	client := New(WithClient(&http.Client{Transport: driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("invalid mutation input unexpectedly reached network: %s", req.URL)
		return nil, errors.New("unreachable")
	})}))

	for name, call := range map[string]func() error{
		"delete-blank":         func() error { return client.Delete(" ") },
		"rename-blank-id":      func() error { return client.Rename(" ", "name") },
		"rename-empty-name":    func() error { return client.Rename("123", "") },
		"move-blank-dest":      func() error { return client.Move(" ", "123") },
		"move-blank-source":    func() error { return client.Move("0", " ") },
		"copy-blank-dest":      func() error { return client.Copy(" ", "123") },
		"copy-blank-source":    func() error { return client.Copy("0", " ") },
		"recycle-clean-blank":  func() error { return client.CleanRecycleBin("", " ") },
		"recycle-revert-blank": func() error { return client.RevertRecycleBin(" ") },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, ErrWrongParams) {
				t.Fatalf("invalid mutation input error = %v, want ErrWrongParams", err)
			}
		})
	}
}

func TestEmptyFileMutationSetsRemainNoOps(t *testing.T) {
	client := New(WithClient(&http.Client{Transport: driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("empty mutation set unexpectedly reached network: %s", req.URL)
		return nil, errors.New("unreachable")
	})}))
	if err := client.Delete(); err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	if err := client.Move("", nil...); err != nil {
		t.Fatalf("Move(empty) = %v", err)
	}
	if err := client.Copy("", nil...); err != nil {
		t.Fatalf("Copy(empty) = %v", err)
	}
}

func TestRecycleListRejectsInvalidPaginationBeforeNetwork(t *testing.T) {
	client := New(WithClient(&http.Client{Transport: driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("invalid recycle pagination unexpectedly reached network: %s", req.URL)
		return nil, errors.New("unreachable")
	})}))
	if _, err := client.ListRecycleBin(-1, 1); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("ListRecycleBin negative offset = %v, want ErrWrongParams", err)
	}
	if _, err := client.ListRecycleBin(0, 0); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("ListRecycleBin zero limit = %v, want ErrWrongParams", err)
	}
}
