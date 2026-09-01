package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yusheng-g/openagent-go/cmd/mcp/iac-server/agent"
	"github.com/yusheng-g/openagent-go/cmd/mcp/iac-server/provider"
	"github.com/yusheng-g/openagent-go/iac"
)

// mockCloud is a CloudProvider that returns dummy credentials instead of
// reading os.Getenv — unit tests must not depend on the user's real cloud
// credentials being present in the environment.
type mockCloud struct{}

func (mockCloud) Name() string                                           { return "mock" }
func (mockCloud) Env() map[string]string                                 { return map[string]string{"HW_REGION": "cn-east-3"} }
func (mockCloud) Skills() fs.FS                                          { return nil }
func (mockCloud) Agents() map[provider.PromptRole]provider.AgentConfig   { return nil }
func (mockCloud) ProviderSource() string                                 { return "mock/mock" }

// TestGetDeploymentStatus_StateV4Address verifies that get_deployment_status
// synthesizes resource addresses from type.name when the terraform state file
// (v4) omits the "address" field — which is what real tfstate looks like.
func TestGetDeploymentStatus_StateV4Address(t *testing.T) {
	// A real terraform state v4 has resources without an "address" key.
	state := `{
		"version": 4,
		"terraform_version": "1.9.8",
		"resources": [
			{"mode":"managed","type":"huaweicloud_compute_instance","name":"wp_server","provider":"provider[\"huaweicloud\"]","instances":[]},
			{"mode":"managed","type":"huaweicloud_vpc","name":"wp_vpc","provider":"provider[\"huaweicloud\"]","instances":[]}
		],
		"outputs": {}
	}`

	dir := t.TempDir()
	deploymentsDir := filepath.Dir(dir)
	depID := filepath.Base(dir)
	if err := os.WriteFile(filepath.Join(dir, "terraform.tfstate"), []byte(state), 0644); err != nil {
		t.Fatal(err)
	}

	tool := &getDeploymentStatusTool{cfg: Config{DeploymentsDir: deploymentsDir}}
	args, _ := json.Marshal(map[string]string{"deployment_id": depID})
	result := tool.Execute(context.Background(), args)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}

	var summary struct {
		ResourceCount int `json:"resource_count"`
		Resources    []struct {
			Address string `json:"address"`
			Type    string `json:"type"`
			Name    string `json:"name"`
		} `json:"resources"`
	}
	if err := json.Unmarshal([]byte(result.Content), &summary); err != nil {
		t.Fatal(err)
	}

	if summary.ResourceCount != 2 {
		t.Fatalf("expected 2 resources, got %d", summary.ResourceCount)
	}
	want := []string{
		"huaweicloud_compute_instance.wp_server",
		"huaweicloud_vpc.wp_vpc",
	}
	for i, w := range want {
		if i >= len(summary.Resources) {
			t.Fatalf("missing resource %d", i)
		}
		if summary.Resources[i].Address != w {
			t.Errorf("resource %d address = %q, want %q", i, summary.Resources[i].Address, w)
		}
	}
}

// TestGetDeploymentStatus_NoStateFile verifies the error when no state exists.
func TestGetDeploymentStatus_NoStateFile(t *testing.T) {
	dir := t.TempDir()
	deploymentsDir := filepath.Dir(dir)
	depID := filepath.Base(dir)

	tool := &getDeploymentStatusTool{cfg: Config{DeploymentsDir: deploymentsDir}}
	args, _ := json.Marshal(map[string]string{"deployment_id": depID})
	result := tool.Execute(context.Background(), args)
	if result.Error == nil {
		t.Fatal("expected error for missing state file")
	}
}

// TestValidDeploymentID exercises the path-traversal guard.
func TestValidDeploymentID(t *testing.T) {
	valid := []string{"d-001", "my-deployment", "d_001", "deployment.1"}
	for _, id := range valid {
		if !validDeploymentID(id) {
			t.Errorf("valid id rejected: %q", id)
		}
	}
	invalid := []string{"", ".", "..", "../etc", "a/b", "a\\b", "..\\etc"}
	for _, id := range invalid {
		if validDeploymentID(id) {
			t.Errorf("invalid id accepted: %q", id)
		}
	}
}

// TestListDeployments_EmptyDirReturnsArray verifies that an empty deployments
// dir marshals to [] (not null) — clients unmarshalling into []T choke on null.
func TestListDeployments_EmptyDirReturnsArray(t *testing.T) {
	dir := t.TempDir()
	tool := &listDeploymentsTool{cfg: Config{DeploymentsDir: dir}}
	result := tool.Execute(context.Background(), nil)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.Content != "[]" {
		t.Fatalf("empty dir should return [], got %s", result.Content)
	}
}

// TestListDeployments_MissingDirReturnsArray verifies the no-dir-yet case.
func TestListDeployments_MissingDirReturnsArray(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nonexistent")
	tool := &listDeploymentsTool{cfg: Config{DeploymentsDir: dir}}
	result := tool.Execute(context.Background(), nil)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.Content != "[]" {
		t.Fatalf("missing dir should return [], got %s", result.Content)
	}
}

// TestDestroyDeployment_InvalidID verifies the path-traversal guard runs
// synchronously before the async job is submitted.
func TestDestroyDeployment_InvalidID(t *testing.T) {
	root := t.TempDir()
	tool := &destroyDeploymentTool{cfg: Config{DeploymentsDir: root, DryRun: true}}
	for _, id := range []string{"", "..", "../etc", "a/b"} {
		args, _ := json.Marshal(map[string]string{"deployment_id": id})
		result := tool.Execute(context.Background(), args)
		if result.Error == nil {
			t.Errorf("invalid id %q should be rejected synchronously", id)
		}
	}
}
// ID sorting — a deployment with terraform.tfstate and tfplan should report
// both true, and output should be sorted by ID.
func TestListDeployments_WithStateAndPlan(t *testing.T) {
	root := t.TempDir()

	// d-002: has state only
	if err := os.MkdirAll(filepath.Join(root, "d-002"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "d-002", "terraform.tfstate"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	// d-001: has both state and plan
	if err := os.MkdirAll(filepath.Join(root, "d-001"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "d-001", "terraform.tfstate"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "d-001", "tfplan"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	// d-003: has neither (only proposed)
	if err := os.MkdirAll(filepath.Join(root, "d-003"), 0755); err != nil {
		t.Fatal(err)
	}

	tool := &listDeploymentsTool{cfg: Config{DeploymentsDir: root}}
	result := tool.Execute(context.Background(), nil)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}

	var deployments []struct {
		ID       string `json:"id"`
		HasState bool   `json:"has_state"`
		HasPlan  bool   `json:"has_plan"`
	}
	if err := json.Unmarshal([]byte(result.Content), &deployments); err != nil {
		t.Fatal(err)
	}
	if len(deployments) != 3 {
		t.Fatalf("expected 3 deployments, got %d", len(deployments))
	}
	// Sorted by ID
	want := []struct {
		ID       string
		HasState bool
		HasPlan  bool
	}{
		{"d-001", true, true},
		{"d-002", true, false},
		{"d-003", false, false},
	}
	for i, w := range want {
		if deployments[i].ID != w.ID {
			t.Errorf("deployment %d id = %q, want %q", i, deployments[i].ID, w.ID)
		}
		if deployments[i].HasState != w.HasState {
			t.Errorf("deployment %s has_state = %v, want %v", w.ID, deployments[i].HasState, w.HasState)
		}
		if deployments[i].HasPlan != w.HasPlan {
			t.Errorf("deployment %s has_plan = %v, want %v", w.ID, deployments[i].HasPlan, w.HasPlan)
		}
	}
}

// TestApplyDeployment_InvalidID verifies the path-traversal guard runs
// synchronously before the async job is submitted.
func TestApplyDeployment_InvalidID(t *testing.T) {
	root := t.TempDir()
	tool := &applyDeploymentTool{cfg: Config{DeploymentsDir: root, DryRun: true}}
	for _, id := range []string{"", "..", "../etc", "a/b"} {
		args, _ := json.Marshal(map[string]string{"deployment_id": id})
		result := tool.Execute(context.Background(), args)
		if result.Error == nil {
			t.Errorf("invalid id %q should be rejected synchronously", id)
		}
	}
}

// Note: apply_deployment's cost gate and destroy_deployment's existence
// check now run inside the async job fn (B26 fix). Testing them requires a
// real Planner (JobManager) — the cost gate logic (HasCost) is covered by
// TestHasCost_InvalidateCost in the agent package, and the async behavior
// is verified end-to-end.

// TestGetDeploymentStatus_WithOutputs verifies that terraform outputs are
// parsed and returned alongside resources.
func TestGetDeploymentStatus_WithOutputs(t *testing.T) {
	state := `{
		"version": 4,
		"resources": [
			{"type":"huaweicloud_compute_instance","name":"web","instances":[]}
		],
		"outputs": {
			"instance_ip": {"value": "192.168.1.100", "type": "string"},
			"vpc_id": {"value": "vpc-abc", "type": "string"}
		}
	}`
	dir := t.TempDir()
	deploymentsDir := filepath.Dir(dir)
	depID := filepath.Base(dir)
	if err := os.WriteFile(filepath.Join(dir, "terraform.tfstate"), []byte(state), 0644); err != nil {
		t.Fatal(err)
	}

	tool := &getDeploymentStatusTool{cfg: Config{DeploymentsDir: deploymentsDir}}
	args, _ := json.Marshal(map[string]string{"deployment_id": depID})
	result := tool.Execute(context.Background(), args)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}

	var summary struct {
		Outputs map[string]struct {
			Value any `json:"value"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal([]byte(result.Content), &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary.Outputs) != 2 {
		t.Fatalf("expected 2 outputs, got %d", len(summary.Outputs))
	}
	if summary.Outputs["instance_ip"].Value != "192.168.1.100" {
		t.Errorf("instance_ip = %v, want 192.168.1.100", summary.Outputs["instance_ip"].Value)
	}
	if summary.Outputs["vpc_id"].Value != "vpc-abc" {
		t.Errorf("vpc_id = %v, want vpc-abc", summary.Outputs["vpc_id"].Value)
	}
}

// ── async apply/destroy (B26) ──
//
// These exercise the full SubmitJob → fn → get_job_result path for apply and
// destroy, including the cost gate and existence check that now run inside
// the async fn. A minimal Planner is built with a real JobManager + DryRun
// cloud; no LLM or real cloud resources are needed.

// newTestPlanner builds a Planner with a real JobManager rooted at workDir,
// DryRun mode, and a mock cloud — no LLM, no real cloud credentials, no real
// resources. Uses mockCloud to avoid reading os.Getenv.
func newTestPlanner(t *testing.T, workDir string) *agent.Planner {
	t.Helper()
	return agent.New(nil, mockCloud{}, nil, nil, nil, workDir, filepath.Join(workDir, "deployments"), true, nil, nil, "")
}

// pollJob waits for a job to reach a terminal status (done/failed/cancelled)
// and returns the job. Fails the test on timeout.
func pollJob(t *testing.T, planner *agent.Planner, jobID string) *agent.Job {
	t.Helper()
	for i := 0; i < 60; i++ {
		job, err := planner.GetJob(context.Background(), jobID, 500*time.Millisecond)
		if err != nil {
			t.Fatalf("GetJob error: %v", err)
		}
		if job == nil {
			t.Fatalf("job %s disappeared", jobID)
		}
		if job.Status != agent.JobRunning {
			return job
		}
	}
	t.Fatalf("job %s did not finish", jobID)
	return nil
}

// extractJobID pulls the job_id from a jobStarted response.
func extractJobID(t *testing.T, content string) string {
	t.Helper()
	var resp struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal([]byte(content), &resp); err != nil {
		t.Fatalf("parse job response: %v (content=%s)", err, content)
	}
	if resp.JobID == "" {
		t.Fatalf("empty job_id in response: %s", content)
	}
	return resp.JobID
}

// mustMkdirAll fails the test if MkdirAll errors — a test setup step that
// cannot proceed without the directory.
func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
}

// mustWriteFile fails the test if WriteFile errors — test fixture data must
// land on disk or the test is meaningless.
func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

// TestApplyDeployment_AsyncStateGateFailure verifies that apply on a
// deployment whose dag.Status is not "planned"/"cost_estimated" (e.g. only
// "proposed") is rejected with the state-gate message — even if cost.json
// exists, a stale plan must not be applied.
func TestApplyDeployment_AsyncStateGateFailure(t *testing.T) {
	workDir := t.TempDir()
	deploymentsDir := filepath.Join(workDir, "deployments")
	mustMkdirAll(t, deploymentsDir)
	depDir := filepath.Join(deploymentsDir, "d-001")
	mustMkdirAll(t, depDir)
	// dag is only "proposed" — not planned, but cost.json exists (stale).
	dagJSON := `{"version":1,"deployment_id":"d-001","status":"proposed","nodes":[{"id":"a","type":"t","name":"a"}]}`
	mustWriteFile(t, filepath.Join(depDir, "dag.json"), []byte(dagJSON))
	mustWriteFile(t, filepath.Join(depDir, "cost.json"), []byte("{}"))
	planner := newTestPlanner(t, workDir)
	tool := &applyDeploymentTool{cfg: Config{
		Planner: planner, Cloud: mockCloud{},
		DeploymentsDir: deploymentsDir, DryRun: true,
	}}

	args, _ := json.Marshal(map[string]string{"deployment_id": "d-001"})
	result := tool.Execute(context.Background(), args)
	if result.Error != nil {
		t.Fatalf("Execute should return job_id, got error: %v", result.Error)
	}
	jobID := extractJobID(t, result.Content)

	job := pollJob(t, planner, jobID)
	if job.Status != agent.JobFailed {
		t.Fatalf("apply on non-planned dag should fail, got status %s", job.Status)
	}
	if !strings.Contains(job.Error, "plannable") {
		t.Fatalf("error should mention plannable state, got: %s", job.Error)
	}
}

// TestApplyDeployment_AsyncCostGateFailure verifies that apply with a planned
// dag but no cost estimate returns a job_id, and the job fails with the
// cost-gate message.
func TestApplyDeployment_AsyncCostGateFailure(t *testing.T) {
	workDir := t.TempDir()
	deploymentsDir := filepath.Join(workDir, "deployments")
	mustMkdirAll(t, deploymentsDir)
	depDir := filepath.Join(deploymentsDir, "d-001")
	mustMkdirAll(t, depDir)
	// dag is planned (passes state gate), but no cost.json (fails cost gate).
	dagJSON := `{"version":1,"deployment_id":"d-001","status":"planned","nodes":[{"id":"a","type":"t","name":"a"}]}`
	mustWriteFile(t, filepath.Join(depDir, "dag.json"), []byte(dagJSON))
	planner := newTestPlanner(t, workDir)
	tool := &applyDeploymentTool{cfg: Config{
		Planner: planner, Cloud: mockCloud{},
		DeploymentsDir: deploymentsDir, DryRun: true,
	}}

	args, _ := json.Marshal(map[string]string{"deployment_id": "d-001"})
	result := tool.Execute(context.Background(), args)
	if result.Error != nil {
		t.Fatalf("Execute should return job_id, got error: %v", result.Error)
	}
	jobID := extractJobID(t, result.Content)

	job := pollJob(t, planner, jobID)
	if job.Status != agent.JobFailed {
		t.Fatalf("apply without cost estimate should fail, got status %s", job.Status)
	}
	if !strings.Contains(job.Error, "cost") {
		t.Fatalf("error should mention cost, got: %s", job.Error)
	}
}

// TestApplyDeployment_AsyncDryRunSuccess verifies apply succeeds in DryRun
// when the cost gate passes (cost.json present). DryRun apply returns a
// simulated result without calling terraform.
func TestApplyDeployment_AsyncDryRunSuccess(t *testing.T) {
	workDir := t.TempDir()
	deploymentsDir := filepath.Join(workDir, "deployments")
	mustMkdirAll(t, deploymentsDir)
	depDir := filepath.Join(deploymentsDir, "d-001")
	mustMkdirAll(t, depDir)
	// Write a cost marker so HasCost passes, and a dag.json with status=planned
	// so the state gate passes.
	mustWriteFile(t, filepath.Join(depDir, "cost.json"), []byte("{}"))
	dagJSON := `{"version":1,"deployment_id":"d-001","status":"planned","nodes":[{"id":"a","type":"t","name":"a"}]}`
	mustWriteFile(t, filepath.Join(depDir, "dag.json"), []byte(dagJSON))
	planner := newTestPlanner(t, workDir)
	tool := &applyDeploymentTool{cfg: Config{
		Planner: planner, Cloud: mockCloud{},
		DeploymentsDir: deploymentsDir, DryRun: true,
	}}

	args, _ := json.Marshal(map[string]string{"deployment_id": "d-001"})
	result := tool.Execute(context.Background(), args)
	if result.Error != nil {
		t.Fatalf("Execute should return job_id, got error: %v", result.Error)
	}
	jobID := extractJobID(t, result.Content)

	job := pollJob(t, planner, jobID)
	if job.Status != agent.JobDone {
		t.Fatalf("apply with cost estimate should succeed in dry-run, got status %s (err=%s)", job.Status, job.Error)
	}
}

// TestDestroyDeployment_AsyncNonexistentFailure verifies that destroy on a
// nonexistent deployment returns a job_id, and the job fails with the
// existence-check message.
func TestDestroyDeployment_AsyncNonexistentFailure(t *testing.T) {
	workDir := t.TempDir()
	deploymentsDir := filepath.Join(workDir, "deployments")
	mustMkdirAll(t, deploymentsDir)
	planner := newTestPlanner(t, workDir)
	tool := &destroyDeploymentTool{cfg: Config{
		Planner: planner, Cloud: mockCloud{},
		DeploymentsDir: deploymentsDir, DryRun: true,
	}}

	args, _ := json.Marshal(map[string]string{"deployment_id": "d-ghost"})
	result := tool.Execute(context.Background(), args)
	if result.Error != nil {
		t.Fatalf("Execute should return job_id, got error: %v", result.Error)
	}
	jobID := extractJobID(t, result.Content)

	job := pollJob(t, planner, jobID)
	if job.Status != agent.JobFailed {
		t.Fatalf("destroy nonexistent should fail, got status %s", job.Status)
	}
	if !strings.Contains(job.Error, "does not exist") {
		t.Fatalf("error should mention 'does not exist', got: %s", job.Error)
	}
}

// TestDestroyDeployment_AsyncDryRunSuccess verifies destroy succeeds in
// DryRun when the deployment directory exists.
func TestDestroyDeployment_AsyncDryRunSuccess(t *testing.T) {
	workDir := t.TempDir()
	deploymentsDir := filepath.Join(workDir, "deployments")
	mustMkdirAll(t, deploymentsDir)
	depDir := filepath.Join(deploymentsDir, "d-001")
	mustMkdirAll(t, depDir)
	// DryRun destroy reads .tf files for resource addresses; an empty dir
	// means no resources, which is a valid "destroyed nothing" result.

	planner := newTestPlanner(t, workDir)
	tool := &destroyDeploymentTool{cfg: Config{
		Planner: planner, Cloud: mockCloud{},
		DeploymentsDir: deploymentsDir, DryRun: true,
	}}

	args, _ := json.Marshal(map[string]string{"deployment_id": "d-001"})
	result := tool.Execute(context.Background(), args)
	if result.Error != nil {
		t.Fatalf("Execute should return job_id, got error: %v", result.Error)
	}
	jobID := extractJobID(t, result.Content)

	job := pollJob(t, planner, jobID)
	if job.Status != agent.JobDone {
		t.Fatalf("destroy existing deployment should succeed in dry-run, got status %s (err=%s)", job.Status, job.Error)
	}
}

// TestApplyDestroy_AsyncSupersession verifies that submitting destroy while
// apply is running for the same deployment supersedes apply (per-deployment
// serialization in JobManager).
func TestApplyDestroy_AsyncSupersession(t *testing.T) {
	workDir := t.TempDir()
	deploymentsDir := filepath.Join(workDir, "deployments")
	mustMkdirAll(t, deploymentsDir)
	depDir := filepath.Join(deploymentsDir, "d-001")
	mustMkdirAll(t, depDir)
	mustWriteFile(t, filepath.Join(depDir, "cost.json"), []byte("{}"))
	dagJSON := `{"version":1,"deployment_id":"d-001","status":"planned","nodes":[{"id":"a","type":"t","name":"a"}]}`
	mustWriteFile(t, filepath.Join(depDir, "dag.json"), []byte(dagJSON))
	planner := newTestPlanner(t, workDir)
	cfg := Config{
		Planner: planner, Cloud: mockCloud{},
		DeploymentsDir: deploymentsDir, DryRun: true,
	}
	applyTool := &applyDeploymentTool{cfg: cfg}
	destroyTool := &destroyDeploymentTool{cfg: cfg}

	args, _ := json.Marshal(map[string]string{"deployment_id": "d-001"})

	// Both submit immediately; destroy supersedes apply (same deployment).
	applyResult := applyTool.Execute(context.Background(), args)
	destroyResult := destroyTool.Execute(context.Background(), args)
	if applyResult.Error != nil || destroyResult.Error != nil {
		t.Fatalf("both should return job_ids")
	}
	applyID := extractJobID(t, applyResult.Content)
	destroyID := extractJobID(t, destroyResult.Content)
	if applyID == destroyID {
		t.Fatal("expected different job ids")
	}

	// Apply should be cancelled (superseded), destroy should complete.
	applyJob := pollJob(t, planner, applyID)
	if applyJob.Status != agent.JobCancelled && applyJob.Status != agent.JobFailed {
		t.Fatalf("apply should be superseded (cancelled/failed), got %s", applyJob.Status)
	}
	destroyJob := pollJob(t, planner, destroyID)
	if destroyJob.Status != agent.JobDone {
		t.Fatalf("destroy should succeed, got %s (err=%s)", destroyJob.Status, destroyJob.Error)
	}
}

// TestApplyDeployment_AppliedIsIdempotent verifies the §4.1 fix: a deployment
// already in DagApplied status can be re-applied (idempotent) rather than
// being rejected by the state gate.
func TestApplyDeployment_AppliedIsIdempotent(t *testing.T) {
	workDir := t.TempDir()
	deploymentsDir := filepath.Join(workDir, "deployments")
	mustMkdirAll(t, deploymentsDir)
	depDir := filepath.Join(deploymentsDir, "d-001")
	mustMkdirAll(t, depDir)
	mustWriteFile(t, filepath.Join(depDir, "cost.json"), []byte("{}"))
	dagJSON := `{"version":1,"deployment_id":"d-001","status":"applied","nodes":[{"id":"a","type":"t","name":"a"}]}`
	mustWriteFile(t, filepath.Join(depDir, "dag.json"), []byte(dagJSON))
	planner := newTestPlanner(t, workDir)
	tool := &applyDeploymentTool{cfg: Config{
		Planner: planner, Cloud: mockCloud{},
		DeploymentsDir: deploymentsDir, DryRun: true,
	}}

	args, _ := json.Marshal(map[string]string{"deployment_id": "d-001"})
	result := tool.Execute(context.Background(), args)
	if result.Error != nil {
		t.Fatalf("Execute should return job_id, got error: %v", result.Error)
	}
	jobID := extractJobID(t, result.Content)

	job := pollJob(t, planner, jobID)
	if job.Status != agent.JobDone {
		t.Fatalf("re-apply on applied deployment should succeed (idempotent), got status %s (err=%s)", job.Status, job.Error)
	}
}

// TestApplyDeployment_DestroyedIsRejected verifies the state gate rejects a
// destroyed deployment with a clear message.
func TestApplyDeployment_DestroyedIsRejected(t *testing.T) {
	workDir := t.TempDir()
	deploymentsDir := filepath.Join(workDir, "deployments")
	mustMkdirAll(t, deploymentsDir)
	depDir := filepath.Join(deploymentsDir, "d-001")
	mustMkdirAll(t, depDir)
	mustWriteFile(t, filepath.Join(depDir, "cost.json"), []byte("{}"))
	dagJSON := `{"version":1,"deployment_id":"d-001","status":"destroyed","nodes":[{"id":"a","type":"t","name":"a"}]}`
	mustWriteFile(t, filepath.Join(depDir, "dag.json"), []byte(dagJSON))
	planner := newTestPlanner(t, workDir)
	tool := &applyDeploymentTool{cfg: Config{
		Planner: planner, Cloud: mockCloud{},
		DeploymentsDir: deploymentsDir, DryRun: true,
	}}

	args, _ := json.Marshal(map[string]string{"deployment_id": "d-001"})
	result := tool.Execute(context.Background(), args)
	jobID := extractJobID(t, result.Content)

	job := pollJob(t, planner, jobID)
	if job.Status != agent.JobFailed {
		t.Fatalf("apply on destroyed deployment should fail, got status %s", job.Status)
	}
	if !strings.Contains(job.Error, "destroyed") {
		t.Fatalf("error should mention destroyed, got: %s", job.Error)
	}
}

// TestDestroyDeployment_StateGateRejectsProposed verifies the §4.2 fix:
// destroy on a deployment that was never applied (status=proposed) is rejected
// with a clear message rather than running terraform destroy as a misleading
// no-op.
func TestDestroyDeployment_StateGateRejectsProposed(t *testing.T) {
	workDir := t.TempDir()
	deploymentsDir := filepath.Join(workDir, "deployments")
	mustMkdirAll(t, deploymentsDir)
	depDir := filepath.Join(deploymentsDir, "d-001")
	mustMkdirAll(t, depDir)
	dagJSON := `{"version":1,"deployment_id":"d-001","status":"proposed","nodes":[{"id":"a","type":"t","name":"a"}]}`
	mustWriteFile(t, filepath.Join(depDir, "dag.json"), []byte(dagJSON))
	planner := newTestPlanner(t, workDir)
	tool := &destroyDeploymentTool{cfg: Config{
		Planner: planner, Cloud: mockCloud{},
		DeploymentsDir: deploymentsDir, DryRun: true,
	}}

	args, _ := json.Marshal(map[string]string{"deployment_id": "d-001"})
	result := tool.Execute(context.Background(), args)
	jobID := extractJobID(t, result.Content)

	job := pollJob(t, planner, jobID)
	if job.Status != agent.JobFailed {
		t.Fatalf("destroy on proposed deployment should fail, got status %s", job.Status)
	}
	if !strings.Contains(job.Error, "never applied") {
		t.Fatalf("error should mention 'never applied', got: %s", job.Error)
	}
}

// TestDestroyDeployment_StateGateRejectsAlreadyDestroyed verifies destroy on
// an already-destroyed deployment is rejected.
func TestDestroyDeployment_StateGateRejectsAlreadyDestroyed(t *testing.T) {
	workDir := t.TempDir()
	deploymentsDir := filepath.Join(workDir, "deployments")
	mustMkdirAll(t, deploymentsDir)
	depDir := filepath.Join(deploymentsDir, "d-001")
	mustMkdirAll(t, depDir)
	dagJSON := `{"version":1,"deployment_id":"d-001","status":"destroyed","nodes":[{"id":"a","type":"t","name":"a"}]}`
	mustWriteFile(t, filepath.Join(depDir, "dag.json"), []byte(dagJSON))
	planner := newTestPlanner(t, workDir)
	tool := &destroyDeploymentTool{cfg: Config{
		Planner: planner, Cloud: mockCloud{},
		DeploymentsDir: deploymentsDir, DryRun: true,
	}}

	args, _ := json.Marshal(map[string]string{"deployment_id": "d-001"})
	result := tool.Execute(context.Background(), args)
	jobID := extractJobID(t, result.Content)

	job := pollJob(t, planner, jobID)
	if job.Status != agent.JobFailed {
		t.Fatalf("destroy on already-destroyed deployment should fail, got status %s", job.Status)
	}
	if !strings.Contains(job.Error, "already destroyed") {
		t.Fatalf("error should mention 'already destroyed', got: %s", job.Error)
	}
}

// ── check_drift ──

// TestCheckDrift_InvalidID verifies the path-traversal guard runs synchronously
// before the async job is submitted.
func TestCheckDrift_InvalidID(t *testing.T) {
	root := t.TempDir()
	tool := &checkDriftTool{cfg: Config{DeploymentsDir: root, DryRun: true}}
	for _, id := range []string{"", "..", "../etc", "a/b"} {
		args, _ := json.Marshal(map[string]string{"deployment_id": id})
		result := tool.Execute(context.Background(), args)
		if result.Error == nil {
			t.Errorf("invalid id %q should be rejected synchronously", id)
		}
	}
}

// TestCheckDrift_StateGateRejectsNonApplied verifies check_drift is rejected
// when dag.Status is not "applied" — drift only makes sense after apply.
func TestCheckDrift_StateGateRejectsNonApplied(t *testing.T) {
	workDir := t.TempDir()
	deploymentsDir := filepath.Join(workDir, "deployments")
	mustMkdirAll(t, deploymentsDir)
	depDir := filepath.Join(deploymentsDir, "d-001")
	mustMkdirAll(t, depDir)
	dagJSON := `{"version":1,"deployment_id":"d-001","status":"cost_estimated","nodes":[{"id":"a","type":"t","name":"a"}]}`
	mustWriteFile(t, filepath.Join(depDir, "dag.json"), []byte(dagJSON))
	planner := newTestPlanner(t, workDir)
	tool := &checkDriftTool{cfg: Config{
		Planner: planner, Cloud: mockCloud{},
		DeploymentsDir: deploymentsDir, DryRun: true,
	}}

	args, _ := json.Marshal(map[string]string{"deployment_id": "d-001"})
	result := tool.Execute(context.Background(), args)
	if result.Error != nil {
		t.Fatalf("Execute should return job_id, got error: %v", result.Error)
	}
	jobID := extractJobID(t, result.Content)

	job := pollJob(t, planner, jobID)
	if job.Status != agent.JobFailed {
		t.Fatalf("check_drift on non-applied dag should fail, got status %s", job.Status)
	}
	if !strings.Contains(job.Error, "has not been applied") {
		t.Fatalf("error should mention 'has not been applied', got: %s", job.Error)
	}
}

// TestCheckDrift_StateGateRejectsDestroyed verifies check_drift is rejected
// when the deployment was already destroyed.
func TestCheckDrift_StateGateRejectsDestroyed(t *testing.T) {
	workDir := t.TempDir()
	deploymentsDir := filepath.Join(workDir, "deployments")
	mustMkdirAll(t, deploymentsDir)
	depDir := filepath.Join(deploymentsDir, "d-001")
	mustMkdirAll(t, depDir)
	dagJSON := `{"version":1,"deployment_id":"d-001","status":"destroyed","nodes":[{"id":"a","type":"t","name":"a"}]}`
	mustWriteFile(t, filepath.Join(depDir, "dag.json"), []byte(dagJSON))
	planner := newTestPlanner(t, workDir)
	tool := &checkDriftTool{cfg: Config{
		Planner: planner, Cloud: mockCloud{},
		DeploymentsDir: deploymentsDir, DryRun: true,
	}}

	args, _ := json.Marshal(map[string]string{"deployment_id": "d-001"})
	result := tool.Execute(context.Background(), args)
	jobID := extractJobID(t, result.Content)

	job := pollJob(t, planner, jobID)
	if job.Status != agent.JobFailed {
		t.Fatalf("check_drift on destroyed dag should fail, got status %s", job.Status)
	}
	if !strings.Contains(job.Error, "has not been applied") {
		t.Fatalf("error should mention 'has not been applied', got: %s", job.Error)
	}
}

// TestCheckDrift_StateGateRejectsMissingDag verifies check_drift is rejected
// when dag.json is missing (empty status from DagStatusOf).
func TestCheckDrift_StateGateRejectsMissingDag(t *testing.T) {
	workDir := t.TempDir()
	deploymentsDir := filepath.Join(workDir, "deployments")
	mustMkdirAll(t, deploymentsDir)
	depDir := filepath.Join(deploymentsDir, "d-001")
	mustMkdirAll(t, depDir)
	// No dag.json — DagStatusOf returns ""
	planner := newTestPlanner(t, workDir)
	tool := &checkDriftTool{cfg: Config{
		Planner: planner, Cloud: mockCloud{},
		DeploymentsDir: deploymentsDir, DryRun: true,
	}}

	args, _ := json.Marshal(map[string]string{"deployment_id": "d-001"})
	result := tool.Execute(context.Background(), args)
	jobID := extractJobID(t, result.Content)

	job := pollJob(t, planner, jobID)
	if job.Status != agent.JobFailed {
		t.Fatalf("check_drift on missing dag should fail, got status %s", job.Status)
	}
	if !strings.Contains(job.Error, "has not been applied") {
		t.Fatalf("error should mention 'has not been applied', got: %s", job.Error)
	}
}

// TestCheckDrift_DryRunClean verifies check_drift in dry-run mode returns
// status "clean" and persists drift.json to disk.
func TestCheckDrift_DryRunClean(t *testing.T) {
	workDir := t.TempDir()
	deploymentsDir := filepath.Join(workDir, "deployments")
	mustMkdirAll(t, deploymentsDir)
	depDir := filepath.Join(deploymentsDir, "d-001")
	mustMkdirAll(t, depDir)
	dagJSON := `{"version":1,"deployment_id":"d-001","status":"applied","nodes":[{"id":"a","type":"t","name":"a"}]}`
	mustWriteFile(t, filepath.Join(depDir, "dag.json"), []byte(dagJSON))
	planner := newTestPlanner(t, workDir)
	tool := &checkDriftTool{cfg: Config{
		Planner: planner, Cloud: mockCloud{},
		DeploymentsDir: deploymentsDir, DryRun: true,
	}}

	args, _ := json.Marshal(map[string]string{"deployment_id": "d-001"})
	result := tool.Execute(context.Background(), args)
	if result.Error != nil {
		t.Fatalf("Execute should return job_id, got error: %v", result.Error)
	}
	jobID := extractJobID(t, result.Content)

	job := pollJob(t, planner, jobID)
	if job.Status != agent.JobDone {
		t.Fatalf("check_drift on applied deployment in dry-run should succeed, got status %s (err=%s)", job.Status, job.Error)
	}

	// Verify the job result content.
	var driftResult agent.DriftResult
	if err := json.Unmarshal(job.Result, &driftResult); err != nil {
		t.Fatalf("parse drift result: %v", err)
	}
	if driftResult.Status != agent.DriftClean {
		t.Errorf("expected status=clean, got %q", driftResult.Status)
	}
	if driftResult.Version != agent.DriftVersion {
		t.Errorf("expected version=%d, got %d", agent.DriftVersion, driftResult.Version)
	}
	if driftResult.DeploymentID != "d-001" {
		t.Errorf("expected deployment_id=d-001, got %q", driftResult.DeploymentID)
	}

	// Verify drift.json was persisted to disk.
	driftData, err := os.ReadFile(filepath.Join(depDir, "drift.json"))
	if err != nil {
		t.Fatalf("drift.json not written: %v", err)
	}
	var onDisk agent.DriftResult
	if err := json.Unmarshal(driftData, &onDisk); err != nil {
		t.Fatalf("parse drift.json: %v", err)
	}
	if onDisk.Status != agent.DriftClean {
		t.Errorf("drift.json status = %q, want clean", onDisk.Status)
	}
}

// TestCheckDrift_DoesNotInvalidateCost verifies check_drift does not delete
// cost.json — drift detection does not change the desired state, so the cost
// estimate remains valid.
func TestCheckDrift_DoesNotInvalidateCost(t *testing.T) {
	workDir := t.TempDir()
	deploymentsDir := filepath.Join(workDir, "deployments")
	mustMkdirAll(t, deploymentsDir)
	depDir := filepath.Join(deploymentsDir, "d-001")
	mustMkdirAll(t, depDir)
	dagJSON := `{"version":1,"deployment_id":"d-001","status":"applied","nodes":[{"id":"a","type":"t","name":"a"}]}`
	mustWriteFile(t, filepath.Join(depDir, "dag.json"), []byte(dagJSON))
	mustWriteFile(t, filepath.Join(depDir, "cost.json"), []byte("{}"))
	planner := newTestPlanner(t, workDir)
	tool := &checkDriftTool{cfg: Config{
		Planner: planner, Cloud: mockCloud{},
		DeploymentsDir: deploymentsDir, DryRun: true,
	}}

	args, _ := json.Marshal(map[string]string{"deployment_id": "d-001"})
	result := tool.Execute(context.Background(), args)
	jobID := extractJobID(t, result.Content)
	job := pollJob(t, planner, jobID)
	if job.Status != agent.JobDone {
		t.Fatalf("check_drift should succeed, got status %s (err=%s)", job.Status, job.Error)
	}

	// cost.json must still exist.
	if !agent.HasCost(depDir) {
		t.Fatal("check_drift must not invalidate cost.json — cost estimate stays valid")
	}
}

// TestCheckDrift_DoesNotChangeDagStatus verifies check_drift does not modify
// dag.Status — drift is a read-only side observation, not a lifecycle advance.
func TestCheckDrift_DoesNotChangeDagStatus(t *testing.T) {
	workDir := t.TempDir()
	deploymentsDir := filepath.Join(workDir, "deployments")
	mustMkdirAll(t, deploymentsDir)
	depDir := filepath.Join(deploymentsDir, "d-001")
	mustMkdirAll(t, depDir)
	dagJSON := `{"version":1,"deployment_id":"d-001","status":"applied","nodes":[{"id":"a","type":"t","name":"a"}]}`
	mustWriteFile(t, filepath.Join(depDir, "dag.json"), []byte(dagJSON))
	planner := newTestPlanner(t, workDir)
	tool := &checkDriftTool{cfg: Config{
		Planner: planner, Cloud: mockCloud{},
		DeploymentsDir: deploymentsDir, DryRun: true,
	}}

	args, _ := json.Marshal(map[string]string{"deployment_id": "d-001"})
	result := tool.Execute(context.Background(), args)
	jobID := extractJobID(t, result.Content)
	job := pollJob(t, planner, jobID)
	if job.Status != agent.JobDone {
		t.Fatalf("check_drift should succeed, got status %s (err=%s)", job.Status, job.Error)
	}

	// dag.Status must still be "applied".
	status := agent.DagStatusOf(depDir)
	if status != agent.DagApplied {
		t.Fatalf("dag.Status = %q, want applied — check_drift must not advance the state machine", status)
	}
}

// TestListDeployments_DriftStatus verifies list_deployments reports the
// drift_status field from drift.json.
func TestListDeployments_DriftStatus(t *testing.T) {
	root := t.TempDir()

	// d-001: has drift.json with status=clean
	mustMkdirAll(t, filepath.Join(root, "d-001"))
	driftJSON := `{"version":1,"deployment_id":"d-001","status":"clean","checked_at":"2026-09-01T00:00:00Z"}`
	mustWriteFile(t, filepath.Join(root, "d-001", "drift.json"), []byte(driftJSON))

	// d-002: no drift.json — drift_status should be ""
	mustMkdirAll(t, filepath.Join(root, "d-002"))

	// d-003: has drift.json with status=drifted
	mustMkdirAll(t, filepath.Join(root, "d-003"))
	driftJSON2 := `{"version":1,"deployment_id":"d-003","status":"drifted","checked_at":"2026-09-01T00:00:00Z"}`
	mustWriteFile(t, filepath.Join(root, "d-003", "drift.json"), []byte(driftJSON2))

	tool := &listDeploymentsTool{cfg: Config{DeploymentsDir: root}}
	result := tool.Execute(context.Background(), nil)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}

	var deployments []struct {
		ID          string `json:"id"`
		DriftStatus string `json:"drift_status"`
	}
	if err := json.Unmarshal([]byte(result.Content), &deployments); err != nil {
		t.Fatal(err)
	}
	if len(deployments) != 3 {
		t.Fatalf("expected 3 deployments, got %d", len(deployments))
	}
	want := []struct {
		ID          string
		DriftStatus string
	}{
		{"d-001", "clean"},
		{"d-002", ""},
		{"d-003", "drifted"},
	}
	for i, w := range want {
		if deployments[i].ID != w.ID {
			t.Errorf("deployment %d id = %q, want %q", i, deployments[i].ID, w.ID)
		}
		if deployments[i].DriftStatus != w.DriftStatus {
			t.Errorf("deployment %s drift_status = %q, want %q", w.ID, deployments[i].DriftStatus, w.DriftStatus)
		}
	}
}

// TestCheckDrift_PlanFnDrifted verifies the non-dry-run DriftDetected branch:
// when the injected planFn returns a plan with changes, check_drift reports
// status=drifted and persists the changes to drift.json.
func TestCheckDrift_PlanFnDrifted(t *testing.T) {
	workDir := t.TempDir()
	deploymentsDir := filepath.Join(workDir, "deployments")
	mustMkdirAll(t, deploymentsDir)
	depDir := filepath.Join(deploymentsDir, "d-001")
	mustMkdirAll(t, depDir)
	dagJSON := `{"version":1,"deployment_id":"d-001","status":"applied","nodes":[{"id":"a","type":"t","name":"a"}]}`
	mustWriteFile(t, filepath.Join(depDir, "dag.json"), []byte(dagJSON))
	planner := newTestPlanner(t, workDir)

	// Inject a plan that returns one update change — simulates drift.
	planFn := func(_ context.Context) (*iac.Plan, error) {
		return &iac.Plan{
			Summary: iac.Summary{Update: 1, Noop: 2},
			Changes: []iac.ResourceChange{
				{Address: "t.a", Type: "t", Action: iac.ActionUpdate},
			},
		}, nil
	}

	tool := &checkDriftTool{
		cfg: Config{
			Planner: planner, Cloud: mockCloud{},
			DeploymentsDir: deploymentsDir, DryRun: false,
		},
		planFn: planFn,
	}

	args, _ := json.Marshal(map[string]string{"deployment_id": "d-001"})
	result := tool.Execute(context.Background(), args)
	if result.Error != nil {
		t.Fatalf("Execute should return job_id, got error: %v", result.Error)
	}
	jobID := extractJobID(t, result.Content)
	job := pollJob(t, planner, jobID)
	if job.Status != agent.JobDone {
		t.Fatalf("check_drift should succeed, got status %s (err=%s)", job.Status, job.Error)
	}

	var driftResult agent.DriftResult
	if err := json.Unmarshal(job.Result, &driftResult); err != nil {
		t.Fatalf("parse drift result: %v", err)
	}
	if driftResult.Status != agent.DriftDetected {
		t.Errorf("expected status=drifted, got %q", driftResult.Status)
	}
	if driftResult.Summary.Update != 1 {
		t.Errorf("expected summary.update=1, got %d", driftResult.Summary.Update)
	}
	if len(driftResult.Changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(driftResult.Changes))
	}
	if driftResult.Changes[0].Address != "t.a" {
		t.Errorf("expected change address=t.a, got %q", driftResult.Changes[0].Address)
	}
	if driftResult.Changes[0].Action != "update" {
		t.Errorf("expected change action=update, got %q", driftResult.Changes[0].Action)
	}

	// Verify drift.json on disk.
	driftData, err := os.ReadFile(filepath.Join(depDir, "drift.json"))
	if err != nil {
		t.Fatalf("drift.json not written: %v", err)
	}
	var onDisk agent.DriftResult
	if err := json.Unmarshal(driftData, &onDisk); err != nil {
		t.Fatalf("parse drift.json: %v", err)
	}
	if onDisk.Status != agent.DriftDetected {
		t.Errorf("drift.json status = %q, want drifted", onDisk.Status)
	}
}

// TestCheckDrift_PlanFnCheckFailed verifies the non-dry-run DriftCheckFailed
// branch: when the injected planFn returns an error, check_drift persists
// check_failed to drift.json and returns the error to the caller.
func TestCheckDrift_PlanFnCheckFailed(t *testing.T) {
	workDir := t.TempDir()
	deploymentsDir := filepath.Join(workDir, "deployments")
	mustMkdirAll(t, deploymentsDir)
	depDir := filepath.Join(deploymentsDir, "d-001")
	mustMkdirAll(t, depDir)
	dagJSON := `{"version":1,"deployment_id":"d-001","status":"applied","nodes":[{"id":"a","type":"t","name":"a"}]}`
	mustWriteFile(t, filepath.Join(depDir, "dag.json"), []byte(dagJSON))
	planner := newTestPlanner(t, workDir)

	planFn := func(_ context.Context) (*iac.Plan, error) {
		return nil, fmt.Errorf("simulated refresh failure: provider timeout")
	}

	tool := &checkDriftTool{
		cfg: Config{
			Planner: planner, Cloud: mockCloud{},
			DeploymentsDir: deploymentsDir, DryRun: false,
		},
		planFn: planFn,
	}

	args, _ := json.Marshal(map[string]string{"deployment_id": "d-001"})
	result := tool.Execute(context.Background(), args)
	jobID := extractJobID(t, result.Content)
	job := pollJob(t, planner, jobID)
	if job.Status != agent.JobFailed {
		t.Fatalf("check_drift should fail on plan error, got status %s", job.Status)
	}
	if !strings.Contains(job.Error, "refresh failure") {
		t.Fatalf("error should contain the plan error, got: %s", job.Error)
	}

	// Verify drift.json persisted with check_failed.
	driftData, err := os.ReadFile(filepath.Join(depDir, "drift.json"))
	if err != nil {
		t.Fatalf("drift.json not written: %v", err)
	}
	var onDisk agent.DriftResult
	if err := json.Unmarshal(driftData, &onDisk); err != nil {
		t.Fatalf("parse drift.json: %v", err)
	}
	if onDisk.Status != agent.DriftCheckFailed {
		t.Errorf("drift.json status = %q, want check_failed", onDisk.Status)
	}
	if !strings.Contains(onDisk.Error, "refresh failure") {
		t.Errorf("drift.json error should contain the plan error, got: %q", onDisk.Error)
	}
}

// FuzzValidDeploymentID_NoTraversal verifies that any id passing
// validDeploymentID cannot escape the deployments dir via filepath traversal
// — workDir(deploymentsDir, id) must always stay inside deploymentsDir.
func FuzzValidDeploymentID_NoTraversal(f *testing.F) {
	f.Add("d-001")
	f.Add("..")
	f.Add("../etc")
	f.Add("a/b")
	f.Add("....//etc")
	f.Add("d-001")
	for _, seed := range []string{
		"..", "../", "..\\", ".",
		"a/../../b", "d/./e",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, id string) {
		if !validDeploymentID(id) {
			return // only check ids that pass the guard
		}
		root := "/safe/root"
		joined := filepath.Join(root, id)
		// The joined path must stay under root — no escaping upward.
		if joined == root || len(joined) <= len(root) {
			t.Fatalf("id %q passed guard but joins to %q (escapes root)", id, joined)
		}
		rel, err := filepath.Rel(root, joined)
		if err != nil {
			t.Fatalf("filepath.Rel error for id %q: %v", id, err)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("id %q passed guard but resolves outside root (rel=%q)", id, rel)
		}
	})
}
