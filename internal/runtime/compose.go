package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/persys/compute-agent/pkg/models"
	"github.com/sirupsen/logrus"
)

// ComposeRuntime manages Docker Compose workloads
type ComposeRuntime struct {
	composeBinary string
	workDir       string
	logger        *logrus.Entry
}

// NewComposeRuntime creates a new Docker Compose runtime
func NewComposeRuntime(composeBinary, workDir string, logger *logrus.Logger) (*ComposeRuntime, error) {
	if composeBinary == "" {
		composeBinary = "docker compose"
	}

	if workDir == "" {
		workDir = "/var/lib/persys/compose"
	}

	// Ensure work directory exists
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create compose work directory: %w", err)
	}

	// Check if docker-compose is available
	parts := strings.Fields(composeBinary)
	if _, err := exec.LookPath(parts[0]); err != nil {
		return nil, fmt.Errorf("docker compose binary not found: %w", err)
	}

	return &ComposeRuntime{
		composeBinary: composeBinary,
		workDir:       workDir,
		logger:        logger.WithField("runtime", "compose"),
	}, nil
}

func (c *ComposeRuntime) Type() models.WorkloadType {
	return models.WorkloadTypeCompose
}

func (c *ComposeRuntime) Create(ctx context.Context, workload *models.Workload) error {
	spec, err := c.parseSpec(workload.Spec)
	if err != nil {
		return fmt.Errorf("failed to parse compose spec: %w", err)
	}

	// Decode the compose YAML
	composeYAML, err := base64.StdEncoding.DecodeString(spec.ComposeYAML)
	if err != nil {
		return fmt.Errorf("failed to decode compose yaml: %w", err)
	}

	// Create project directory
	projectDir := filepath.Join(c.workDir, workload.ID)
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		return fmt.Errorf("failed to create project directory: %w", err)
	}

	// Write docker-compose.yml
	composeFile := filepath.Join(projectDir, "docker-compose.yml")
	if err := os.WriteFile(composeFile, composeYAML, 0644); err != nil {
		return fmt.Errorf("failed to write compose file: %w", err)
	}

	// Write .env file if environment variables are provided
	if len(spec.Env) > 0 {
		envFile := filepath.Join(projectDir, ".env")
		envContent := c.buildEnvFile(spec.Env)
		if err := os.WriteFile(envFile, []byte(envContent), 0644); err != nil {
			return fmt.Errorf("failed to write env file: %w", err)
		}
	}

	c.logger.Infof("Created compose project: %s", workload.ID)
	return nil
}

func (c *ComposeRuntime) Start(ctx context.Context, id string) error {
	projectDir := filepath.Join(c.workDir, id)

	cmd := c.buildCommand(ctx, "-p", id, "up", "-d")
	cmd.Dir = projectDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to start compose project: %w, output: %s", err, string(output))
	}

	c.logger.Infof("Started compose project: %s", id)
	return nil
}

func (c *ComposeRuntime) Stop(ctx context.Context, id string) error {
	projectDir := filepath.Join(c.workDir, id)

	cmd := c.buildCommand(ctx, "-p", id, "stop")
	cmd.Dir = projectDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to stop compose project: %w, output: %s", err, string(output))
	}

	c.logger.Infof("Stopped compose project: %s", id)
	return nil
}

func (c *ComposeRuntime) Delete(ctx context.Context, id string) error {
	projectDir := filepath.Join(c.workDir, id)

	// Stop and remove containers
	cmd := c.buildCommand(ctx, "-p", id, "down", "-v")
	cmd.Dir = projectDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		c.logger.Warnf("Failed to bring down compose project: %v, output: %s", err, string(output))
	}

	// Remove project directory
	if err := os.RemoveAll(projectDir); err != nil {
		return fmt.Errorf("failed to remove project directory: %w", err)
	}

	c.logger.Infof("Deleted compose project: %s", id)
	return nil
}

func (c *ComposeRuntime) Status(ctx context.Context, id string) (models.ActualState, string, error) {
	projectDir := filepath.Join(c.workDir, id)

	// Check if project directory exists
	if _, err := os.Stat(projectDir); os.IsNotExist(err) {
		return models.ActualStateUnknown, "project not found", nil
	}

	// Get project status
	cmd := c.buildCommand(ctx, "-p", id, "ps", "-q")
	cmd.Dir = projectDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		return models.ActualStateUnknown, "", fmt.Errorf("failed to get compose status: %w", err)
	}

	// Count running containers
	containerIDs := strings.Split(strings.TrimSpace(string(output)), "\n")
	runningCount := 0

	for _, containerID := range containerIDs {
		if containerID == "" {
			continue
		}

		// Check if container is running
		inspectCmd := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Status}}", containerID)
		inspectOutput, err := inspectCmd.CombinedOutput()
		if err != nil {
			continue
		}

		status := strings.TrimSpace(string(inspectOutput))
		if status == "running" {
			runningCount++
		}
	}

	totalContainers := len(containerIDs)
	if totalContainers > 0 && containerIDs[0] == "" {
		totalContainers = 0
	}

	if totalContainers == 0 {
		return models.ActualStateStopped, "no containers", nil
	}

	if runningCount == totalContainers {
		return models.ActualStateRunning, fmt.Sprintf("%d/%d containers running", runningCount, totalContainers), nil
	} else if runningCount > 0 {
		return models.ActualStatePending, fmt.Sprintf("%d/%d containers running", runningCount, totalContainers), nil
	}

	return models.ActualStateStopped, fmt.Sprintf("0/%d containers running", totalContainers), nil
}

func (c *ComposeRuntime) List(ctx context.Context) ([]string, error) {
	entries, err := os.ReadDir(c.workDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("failed to list compose projects: %w", err)
	}

	var projects []string
	for _, entry := range entries {
		if entry.IsDir() {
			projects = append(projects, entry.Name())
		}
	}

	return projects, nil
}

func (c *ComposeRuntime) Healthy(ctx context.Context) error {
	parts := strings.Fields(c.composeBinary)
	args := append(parts[1:], "version")
	cmd := exec.CommandContext(ctx, parts[0], args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker-compose not healthy: %w", err)
	}
	return nil
}

// Helper functions

func (c *ComposeRuntime) buildCommand(ctx context.Context, args ...string) *exec.Cmd {
	parts := strings.Fields(c.composeBinary)
	allArgs := append(parts[1:], args...)
	return exec.CommandContext(ctx, parts[0], allArgs...)
}

func (c *ComposeRuntime) parseSpec(specMap map[string]interface{}) (*models.ComposeSpec, error) {
	data, err := json.Marshal(specMap)
	if err != nil {
		return nil, err
	}

	var spec models.ComposeSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, err
	}

	return &spec, nil
}

func (c *ComposeRuntime) buildEnvFile(env map[string]string) string {
	var lines []string
	for k, v := range env {
		lines = append(lines, fmt.Sprintf("%s=%s", k, v))
	}
	return strings.Join(lines, "\n")
}
