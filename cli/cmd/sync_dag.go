package cmd

import (
	syncexecpkg "github.com/SheltonZhu/115driver/internal/syncexec"
)

type syncExecutionGraph = syncexecpkg.Graph

func resolveSyncJobs(jobs int) (int, error) {
	return syncexecpkg.ResolveJobs(jobs)
}

func buildSyncExecutionGraph(plan syncPlan) (syncExecutionGraph, error) {
	return syncexecpkg.BuildGraph(plan)
}

func validateSyncExecutionGraph(graph syncExecutionGraph) error {
	return syncexecpkg.ValidateGraph(graph)
}

func syncItemWritesRemote(item syncPlanItem) bool {
	return syncexecpkg.ItemWritesRemote(item)
}

func syncItemWritesLocal(item syncPlanItem) bool {
	return syncexecpkg.ItemWritesLocal(item)
}

func syncItemCreatesRemoteDirectory(item syncPlanItem) bool {
	return syncexecpkg.ItemCreatesRemoteDirectory(item)
}

func syncItemCreatesLocalDirectory(item syncPlanItem) bool {
	return syncexecpkg.ItemCreatesLocalDirectory(item)
}

func syncPlanFileTransferCount(plan syncPlan) int {
	return syncexecpkg.PlanFileTransferCount(plan)
}

func syncTransferBudget(workersPerInterface, jobs, fileTransfers int) (workersPerTransfer, parallelTransfers int, err error) {
	return syncexecpkg.TransferBudget(workersPerInterface, jobs, fileTransfers)
}

func syncTransferWorkerLimit(workersPerInterface, jobs, fileTransfers int) (int, error) {
	return syncexecpkg.TransferWorkerLimit(workersPerInterface, jobs, fileTransfers)
}

func syncItemUsesFileTransfer(item syncPlanItem) bool {
	return syncexecpkg.ItemUsesFileTransfer(item)
}
