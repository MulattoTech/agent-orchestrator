package sessionmanager

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/testenv"
)

// Set by the offline package build only. Even invoking that executable without
// arguments, or supplying a broader -test.run, cannot run other package tests.
var admissionOfflineCompanion string

const admissionOfflineTestAllowlist = "^(TestAdmissionControlsPersistAcrossRestartAndPreserveWorkers|TestAdmissionControlsRejectInvalidOrCancelledUpdates|TestAdmissionControlsPauseQueueBeforeClaim|TestAdmissionControlsLowerWhileReservationsExist|TestAdmissionNativeWindowsFortyProcessesRecover|TestAdmissionSyntheticFleetStages|TestAdmissionQueueFortyTasksQuotaRestartAndExactlyOnce|TestAdmissionCrashAfterLaunchBeforeAckDoesNotDuplicate|TestAdmissionRestoreContendersAndDirtyPreservation|TestAdmissionProjectCapResourcePauseAndCoordinatorSingularity|TestAdmissionAmbiguousCrashBlocksWithoutAnotherLaunch|TestAdmissionDependenciesAndCancellation|TestAdmissionWorkspaceOwnerConflictDoesNotRemoveDirtyTree|TestAdmissionResourcePauseBlocksStoppedWorkerRestart|TestAdmissionRejectsUnknownSessionKind|TestAdmissionQueuedRestoreUsesLifecycleGeneration|TestAdmissionPRCompletionRefusesIncompleteFacts|TestAdmissionPRCompletionFairDurableAndExactlyOnce|TestAdmissionPRCompletionCannotBypassQueue|TestAdmissionNativeWindowsPhysicalWorkspaceOwnership|TestAdmissionNativeWindowsRestoreRejectsJunctionBeforeWorkspaceMutation|TestSessionHealthDetectsLostChildWithoutMutatingRows|TestSessionHealthFencesUnknownGeneration|TestSessionHealthDistinguishesChatAttachmentFromProcessProof|TestSessionHealthLeavesStoppedSessionIdle|TestSessionHealthBoundsProbeAndHonorsCancellation|TestCleanupAutomaticRetryBoundedAndScheduled|TestCleanupDeferredCompletionCountsOneAttempt|TestCleanupLegacyExhaustedPendingPreservesWorkspace|TestCleanupDirtyRefusalDoesNotAutomaticallyRetry|TestWorkspaceScriptAdmissionContentionAndCancellation|TestWorkspaceScriptAdmissionConcurrentBounds|TestWorkspaceScriptAdmissionFailureReleaseAndNoRetry|TestWorkspaceScriptAdmissionInvalidPolicyFailsClosed)$"

func TestMain(m *testing.M) {
	testenv.ClearSession()
	if admissionOfflineCompanion == "1" {
		flag.Parse()
		for name, value := range map[string]string{
			"test.run": admissionOfflineTestAllowlist, "test.bench": "", "test.fuzz": "",
			"test.count": "1", "test.parallel": "1", "test.timeout": "120s", "test.cpu": "2", "test.v": "true",
		} {
			if err := flag.Set(name, value); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
		}
		runtime.GOMAXPROCS(2)
		go func() {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			started := time.Now()
			for range ticker.C {
				var stats runtime.MemStats
				runtime.ReadMemStats(&stats)
				fmt.Fprintf(os.Stderr, "OFFLINE_FLEET_HEARTBEAT elapsed_seconds=%d goroutines=%d heap_bytes=%d sys_bytes=%d gc_cycles=%d\n",
					int(time.Since(started).Seconds()), runtime.NumGoroutine(), stats.HeapAlloc, stats.Sys, stats.NumGC)
			}
		}()
	}
	os.Exit(m.Run())
}
