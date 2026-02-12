package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/persys/compute-agent/pkg/models"
	"github.com/sirupsen/logrus"
)

// DockerRuntime manages Docker container workloads
type DockerRuntime struct {
	client *client.Client
	logger *logrus.Entry
}

// NewDockerRuntime creates a new Docker runtime
func NewDockerRuntime(endpoint string, logger *logrus.Logger) (*DockerRuntime, error) {
	var cli *client.Client
	var err error

	if endpoint == "" || strings.HasPrefix(endpoint, "unix://") {
		cli, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	} else {
		cli, err = client.NewClientWithOpts(
			client.WithHost(endpoint),
			client.WithAPIVersionNegotiation(),
		)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}

	return &DockerRuntime{
		client: cli,
		logger: logger.WithField("runtime", "docker"),
	}, nil
}

func (d *DockerRuntime) Type() models.WorkloadType {
	return models.WorkloadTypeContainer
}

func (d *DockerRuntime) Create(ctx context.Context, workload *models.Workload) error {
	spec, err := d.parseSpec(workload.Spec)
	if err != nil {
		return fmt.Errorf("failed to parse container spec: %w", err)
	}

	// Pull image with retry logic
	d.logger.Infof("Pulling image: %s (this may take a while)", spec.Image)
	err = d.pullImageWithRetry(ctx, spec.Image, 3)
	if err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}
	d.logger.Infof("Successfully pulled image: %s", spec.Image)

	// Build container config
	containerConfig := &container.Config{
		Image:  spec.Image,
		Cmd:    spec.Command,
		Env:    d.buildEnv(spec.Env),
		Labels: spec.Labels,
	}

	if len(spec.Args) > 0 {
		containerConfig.Cmd = append(containerConfig.Cmd, spec.Args...)
	}

	// Build host config
	hostConfig := &container.HostConfig{
		RestartPolicy: d.buildRestartPolicy(spec.RestartPolicy),
		Mounts:        d.buildMounts(spec.Volumes),
		PortBindings:  d.buildPortBindings(spec.Ports),
	}

	if spec.Resources != nil {
		hostConfig.Resources = container.Resources{
			CPUShares: spec.Resources.CPUShares,
			Memory:    spec.Resources.MemoryBytes,
		}
	}

	// Add exposed ports to config
	if len(spec.Ports) > 0 {
		containerConfig.ExposedPorts = d.buildExposedPorts(spec.Ports)
	}

	// Create container
	resp, err := d.client.ContainerCreate(
		ctx,
		containerConfig,
		hostConfig,
		nil,
		nil,
		workload.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	d.logger.Infof("Created container: %s (%s)", workload.ID, resp.ID)
	return nil
}

func (d *DockerRuntime) Start(ctx context.Context, id string) error {
	if err := d.client.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}
	d.logger.Infof("Started container: %s", id)
	return nil
}

func (d *DockerRuntime) Stop(ctx context.Context, id string) error {
	timeout := 30
	if err := d.client.ContainerStop(ctx, id, container.StopOptions{
		Timeout: &timeout,
	}); err != nil {
		return fmt.Errorf("failed to stop container: %w", err)
	}
	d.logger.Infof("Stopped container: %s", id)
	return nil
}

func (d *DockerRuntime) Delete(ctx context.Context, id string) error {
	// Stop first if running
	_ = d.Stop(ctx, id)

	if err := d.client.ContainerRemove(ctx, id, container.RemoveOptions{
		Force: true,
	}); err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}
	d.logger.Infof("Deleted container: %s", id)
	return nil
}

func (d *DockerRuntime) Status(ctx context.Context, id string) (models.ActualState, string, error) {
	info, err := d.client.ContainerInspect(ctx, id)
	if err != nil {
		if client.IsErrNotFound(err) {
			return models.ActualStateUnknown, "container not found", nil
		}
		return models.ActualStateUnknown, "", fmt.Errorf("failed to inspect container: %w", err)
	}

	state := models.ActualStateUnknown
	message := info.State.Status

	switch {
	case info.State.Running:
		state = models.ActualStateRunning
	case info.State.Paused:
		state = models.ActualStateStopped
		message = "paused"
	case info.State.Restarting:
		state = models.ActualStatePending
		message = "restarting"
	case info.State.Dead:
		state = models.ActualStateFailed
		message = fmt.Sprintf("dead: %s", info.State.Error)
	case info.State.ExitCode != 0:
		state = models.ActualStateFailed
		message = fmt.Sprintf("exited with code %d: %s", info.State.ExitCode, info.State.Error)
	default:
		state = models.ActualStateStopped
	}

	return state, message, nil
}

func (d *DockerRuntime) List(ctx context.Context) ([]string, error) {
	containers, err := d.client.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	var ids []string
	for _, c := range containers {
		// Use names if available, otherwise use ID
		if len(c.Names) > 0 {
			name := strings.TrimPrefix(c.Names[0], "/")
			ids = append(ids, name)
		} else {
			ids = append(ids, c.ID[:12])
		}
	}

	return ids, nil
}

func (d *DockerRuntime) Healthy(ctx context.Context) error {
	_, err := d.client.Ping(ctx)
	return err
}

// Helper functions

func (d *DockerRuntime) pullImageWithRetry(ctx context.Context, image string, maxRetries int) error {
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		d.logger.Debugf("Pulling image %s (attempt %d/%d)", image, attempt, maxRetries)

		reader, err := d.client.ImagePull(ctx, image, types.ImagePullOptions{})
		if err != nil {
			lastErr = err
			if attempt < maxRetries {
				backoff := time.Duration(attempt) * 5 * time.Second
				d.logger.Warnf("Image pull failed (attempt %d/%d): %v, retrying in %s",
					attempt, maxRetries, err, backoff)
				select {
				case <-time.After(backoff):
					// Continue to next attempt
				case <-ctx.Done():
					return fmt.Errorf("context cancelled during image pull retry: %w", ctx.Err())
				}
			} else {
				d.logger.Errorf("Image pull failed after %d attempts: %v", maxRetries, err)
			}
			continue
		}
		defer reader.Close()

		// Consume and log pull output
		_, pullErr := io.Copy(io.Discard, reader)
		if pullErr != nil {
			lastErr = pullErr
			if attempt < maxRetries {
				backoff := time.Duration(attempt) * 5 * time.Second
				d.logger.Warnf("Error reading pull output (attempt %d/%d): %v, retrying in %s",
					attempt, maxRetries, pullErr, backoff)
				select {
				case <-time.After(backoff):
					// Continue to next attempt
				case <-ctx.Done():
					return fmt.Errorf("context cancelled during image pull: %w", ctx.Err())
				}
			}
			continue
		}

		// Success
		d.logger.Infof("Image pull successful: %s", image)
		return nil
	}

	return lastErr
}

func (d *DockerRuntime) parseSpec(specMap map[string]interface{}) (*models.ContainerSpec, error) {
	data, err := json.Marshal(specMap)
	if err != nil {
		return nil, err
	}

	var spec models.ContainerSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, err
	}

	return &spec, nil
}

func (d *DockerRuntime) buildEnv(env map[string]string) []string {
	var result []string
	for k, v := range env {
		result = append(result, fmt.Sprintf("%s=%s", k, v))
	}
	return result
}

func (d *DockerRuntime) buildMounts(volumes []models.VolumeMount) []mount.Mount {
	var mounts []mount.Mount
	for _, v := range volumes {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   v.HostPath,
			Target:   v.ContainerPath,
			ReadOnly: v.ReadOnly,
		})
	}
	return mounts
}

func (d *DockerRuntime) buildPortBindings(ports []models.PortMapping) nat.PortMap {
	portMap := nat.PortMap{}
	for _, p := range ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		containerPort := nat.Port(fmt.Sprintf("%d/%s", p.ContainerPort, proto))
		portMap[containerPort] = []nat.PortBinding{
			{
				HostIP:   "0.0.0.0",
				HostPort: fmt.Sprintf("%d", p.HostPort),
			},
		}
	}
	return portMap
}

func (d *DockerRuntime) buildExposedPorts(ports []models.PortMapping) nat.PortSet {
	exposed := nat.PortSet{}
	for _, p := range ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		port := nat.Port(fmt.Sprintf("%d/%s", p.ContainerPort, proto))
		exposed[port] = struct{}{}
	}
	return exposed
}

func (d *DockerRuntime) buildRestartPolicy(policy *models.RestartPolicy) container.RestartPolicy {
	if policy == nil {
		return container.RestartPolicy{Name: container.RestartPolicyMode("no")}
	}

	return container.RestartPolicy{
		Name:              container.RestartPolicyMode(policy.Policy),
		MaximumRetryCount: policy.MaxRetryCount,
	}
}
