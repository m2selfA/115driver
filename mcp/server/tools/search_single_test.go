package tools

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
)

func TestSearchRejectsInvalidParametersBeforeNetwork(t *testing.T) {
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("invalid single search unexpectedly reached network: %s", req.URL)
		return nil, fmt.Errorf("unreachable")
	})})))
	tools := NewSearchTools(client)
	for name, args := range map[string]SearchArgs{
		"negative-offset": {SearchValue: "x", Offset: -1},
		"negative-limit":  {SearchValue: "x", Limit: -1},
		"oversized-limit": {SearchValue: "x", Limit: maxMCPSearchBatchLimit + 1},
		"invalid-type":    {SearchValue: "x", Type: 7},
		"invalid-asc":     {SearchValue: "x", Asc: 2},
	} {
		t.Run(name, func(t *testing.T) {
			result, _, err := tools.search(context.Background(), nil, args)
			if err != nil || result == nil || !result.IsError {
				t.Fatalf("invalid search result=%#v err=%v args=%#v", result, err, args)
			}
		})
	}
}
