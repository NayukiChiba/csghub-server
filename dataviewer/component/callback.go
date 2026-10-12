package component

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"opencsg.com/csghub-server/builder/git/gitserver"
	"opencsg.com/csghub-server/builder/store/database"
	"opencsg.com/csghub-server/builder/temporal"
	"opencsg.com/csghub-server/common/config"
	"opencsg.com/csghub-server/common/types"
	dvCom "opencsg.com/csghub-server/dataviewer/common"
	"opencsg.com/csghub-server/dataviewer/workflows"
)

type CallbackComponent interface {
	TriggerDataviewUpdateWorkflow(ctx context.Context, req types.UpdateViewerReq) (*types.WorkFlowInfo, error)
}

type callbackComponentImpl struct {
	cfg             *config.Config
	workflowClient  temporal.Client
	gitServer       gitserver.GitServer
	repoStore       database.RepoStore
	dataviewerStore database.DataviewerStore
}

func NewCallbackComponent(cfg *config.Config, tc temporal.Client, gs gitserver.GitServer) (CallbackComponent, error) {
	return &callbackComponentImpl{
		cfg:             cfg,
		workflowClient:  tc,
		gitServer:       gs,
		repoStore:       database.NewRepoStore(),
		dataviewerStore: database.NewDataviewerStore(),
	}, nil
}

func (c *callbackComponentImpl) TriggerDataviewUpdateWorkflow(ctx context.Context, req types.UpdateViewerReq) (*types.WorkFlowInfo, error) {
	if req.Branch == dvCom.ParquetBranch || req.Branch == dvCom.DuckdbBranch {
		return nil, fmt.Errorf("skip update dataviewer for repo %s/%s branch %s changes", req.Namespace, req.Name, req.Branch)
	}
	repo, err := c.repoStore.FindByPath(ctx, req.RepoType, req.Namespace, req.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to find dataset, error: %w", err)
	}
	req.Branch = repo.DefaultBranch
	req.RepoID = repo.ID
	req.Migrated = repo.Migrated

	jobs, err := c.dataviewerStore.GetRunningJobsByRepoID(ctx, req.RepoID)
	if err != nil {
		return nil, fmt.Errorf("failed to get running jobs by repo_id %d, error: %w", req.RepoID, err)
	}
	// A job row in pending/running state does not prove the temporal workflow
	// is alive: worker restarts can orphan jobs, and such zombies would block
	// re-triggering forever. Reconcile with temporal before rejecting.
	running := 0
	for _, job := range jobs {
		alive, err := c.workflowAlive(ctx, job.WorkflowID)
		if err != nil {
			slog.Warn("failed to check workflow status, treat as running", slog.String("workflow_id", job.WorkflowID), slog.Any("error", err))
			running++
			continue
		}
		if alive {
			running++
			continue
		}
		job.Status = types.WorkflowFailed
		job.Logs = "manually marked failed: workflow is not running"
		if _, err := c.dataviewerStore.UpdateJob(ctx, job); err != nil {
			slog.Error("failed to mark zombie job as failed", slog.String("workflow_id", job.WorkflowID), slog.Any("error", err))
		}
	}
	if running > 0 {
		slog.Debug("there are still running jobs for this dataset", slog.Any("req", req), slog.Any("running jobs count", running))
		return nil, fmt.Errorf("there are still running jobs for this dataset %s/%s branch %s", req.Namespace, req.Name, req.Branch)
	}

	dv, err := c.dataviewerStore.GetViewerByRepoID(ctx, repo.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to get dataviewer by repo_id %d, error: %w", repo.ID, err)
	}

	newWorkflowID := workflows.GenerateWorkFlowID(req)

	if dv == nil {
		dataviewer := database.Dataviewer{
			RepoID:     repo.ID,
			RepoPath:   fmt.Sprintf("%s/%s", req.Namespace, repo.Name),
			RepoBranch: req.Branch,
			WorkflowID: newWorkflowID,
		}
		err = c.dataviewerStore.CreateViewer(ctx, dataviewer)
		if err != nil {
			return nil, fmt.Errorf("fail to create dataviewer, error: %w", err)
		}
	} else {
		job := database.DataviewerJob{
			RepoID:     repo.ID,
			WorkflowID: newWorkflowID,
			Status:     types.WorkflowPending,
			AutoCard:   true,
			Logs:       types.WorkflowMsgPending,
		}
		err := c.dataviewerStore.CreateJob(ctx, job)
		if err != nil {
			return nil, fmt.Errorf("fail to create dataviewer job, error: %w", err)
		}
	}

	executeTimeOut := time.Duration(c.cfg.WorkFLow.ExecutionTimeout) * time.Second
	taskTimeout := time.Duration(c.cfg.WorkFLow.TaskTimeout) * time.Second
	options := client.StartWorkflowOptions{
		ID:                       newWorkflowID,
		TaskQueue:                dvCom.TaskQueueDataViewerDatasetUpdate,
		WorkflowIDReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
		WorkflowExecutionTimeout: executeTimeOut,
		WorkflowTaskTimeout:      taskTimeout,
	}
	wfRun, err := c.workflowClient.ExecuteWorkflow(
		context.Background(), options, workflows.DataViewerUpdateWorkflow,
		dvCom.WorkflowUpdateParams{Req: req, Config: c.cfg},
	)
	if err != nil {
		return nil, fmt.Errorf("fail to execute workflow, error: %w", err)
	}

	slog.Info("submit workflow", slog.Any("WorkflowID", wfRun.GetID()), slog.Any("RunID", wfRun.GetRunID()), slog.Any("req", req))

	workFlowInfo := types.WorkFlowInfo{
		Namespace:     req.Namespace,
		Name:          req.Name,
		Branch:        req.Branch,
		RepoType:      req.RepoType,
		WorkFlowID:    wfRun.GetID(),
		WorkFlowRunID: wfRun.GetRunID(),
	}

	return &workFlowInfo, nil
}

// workflowAlive reports whether the temporal workflow is still running.
// A not-found workflow or any closed status means the job row is a zombie.
func (c *callbackComponentImpl) workflowAlive(ctx context.Context, workflowID string) (bool, error) {
	resp, err := c.workflowClient.DescribeWorkflowExecution(ctx, workflowID, "")
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, fmt.Errorf("failed to describe workflow %s, error: %w", workflowID, err)
	}
	return resp.GetWorkflowExecutionInfo().GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, nil
}
