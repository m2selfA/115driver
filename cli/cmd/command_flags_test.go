package cmd

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestRecursiveAndSessionFlagsHaveShortForms(t *testing.T) {
	tests := []struct {
		name      string
		flagName  string
		shorthand string
		lookup    func(string) string
	}{
		{name: "upload recursive", flagName: "recursive", shorthand: "r", lookup: func(name string) string { return uploadCmd.Flags().Lookup(name).Shorthand }},
		{name: "download recursive", flagName: "recursive", shorthand: "r", lookup: func(name string) string { return downloadCmd.Flags().Lookup(name).Shorthand }},
		{name: "ls recursive", flagName: "recursive", shorthand: "R", lookup: func(name string) string { return lsCmd.Flags().Lookup(name).Shorthand }},
		{name: "upload session", flagName: "session", shorthand: "s", lookup: func(name string) string { return uploadCmd.Flags().Lookup(name).Shorthand }},
		{name: "download session", flagName: "session", shorthand: "s", lookup: func(name string) string { return downloadCmd.Flags().Lookup(name).Shorthand }},
		{name: "search directory", flagName: "dir", shorthand: "d", lookup: func(name string) string { return searchCmd.Flags().Lookup(name).Shorthand }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.lookup(test.flagName); got != test.shorthand {
				t.Fatalf("flag --%s shorthand: got %q want %q", test.flagName, got, test.shorthand)
			}
		})
	}
}

func TestTransferCommandsExposeWorkersPerInterfaceOverride(t *testing.T) {
	for name, command := range map[string]*cobra.Command{"upload": uploadCmd, "download": downloadCmd} {
		flag := command.Flags().Lookup("workers-per-interface")
		if flag == nil {
			t.Fatalf("%s command is missing --workers-per-interface", name)
		}
		if flag.Shorthand != "" {
			t.Fatalf("%s --workers-per-interface unexpectedly has shorthand %q", name, flag.Shorthand)
		}
	}
}

func TestBatchRemoteOperationCommandsAcceptMultipleSources(t *testing.T) {
	if err := cpCmd.Args(cpCmd, []string{"a", "b", "/dest"}); err != nil {
		t.Fatalf("cp rejected multiple sources: %v", err)
	}
	if err := mvCmd.Args(mvCmd, []string{"a", "b", "/dest"}); err != nil {
		t.Fatalf("mv rejected multiple sources: %v", err)
	}
	if err := rmCmd.Args(rmCmd, []string{"a", "b"}); err != nil {
		t.Fatalf("rm rejected multiple paths: %v", err)
	}
	if err := cpCmd.Args(cpCmd, []string{"only-one"}); err == nil {
		t.Fatal("cp accepted missing destination")
	}
}
