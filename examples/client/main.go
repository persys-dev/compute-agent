package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	pb "github.com/persys/compute-agent/pkg/api/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	// Parse command line flags
	serverAddr := flag.String("server", "localhost:50051", "Agent server address")
	certFile := flag.String("cert", "", "Client certificate file")
	keyFile := flag.String("key", "", "Client key file")
	caFile := flag.String("ca", "", "CA certificate file")
	insecure := flag.Bool("insecure", false, "Disable TLS")
	action := flag.String("action", "health", "Action to perform: health, apply, apply-compose, apply-vm, status, list, delete")
	workloadID := flag.String("id", "", "Workload ID")
	workloadType := flag.String("type", "container", "Workload type: container, compose, vm")
	flag.Parse()

	// Create gRPC connection
	var opts []grpc.DialOption

	if *insecure {
		opts = append(opts, grpc.WithInsecure())
	} else {
		// Load TLS credentials
		tlsConfig, err := loadTLSConfig(*certFile, *keyFile, *caFile)
		if err != nil {
			log.Fatalf("Failed to load TLS config: %v", err)
		}
		creds := credentials.NewTLS(tlsConfig)
		opts = append(opts, grpc.WithTransportCredentials(creds))
	}

	conn, err := grpc.Dial(*serverAddr, opts...)
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	client := pb.NewAgentServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Execute action
	switch *action {
	case "health":
		healthCheck(ctx, client)
	case "apply":
		switch *workloadType {
		case "container":
			applyWorkload(ctx, client, *workloadID)
		case "compose":
			applyComposeWorkload(ctx, client, *workloadID)
		case "vm":
			applyVMWorkload(ctx, client, *workloadID)
		default:
			log.Fatalf("Unknown workload type: %s", *workloadType)
		}
	case "apply-compose":
		applyComposeWorkload(ctx, client, *workloadID)
	case "apply-vm":
		applyVMWorkload(ctx, client, *workloadID)
	case "status":
		getStatus(ctx, client, *workloadID)
	case "list":
		listWorkloads(ctx, client)
	case "delete":
		deleteWorkload(ctx, client, *workloadID)
	default:
		log.Fatalf("Unknown action: %s", *action)
	}
}

func loadTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	// Load client certificate
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load client certificate: %w", err)
	}

	// Load CA certificate
	caCert, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate: %w", err)
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse CA certificate")
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caCertPool,
		MinVersion:   tls.VersionTLS12,
	}

	return tlsConfig, nil
}

func healthCheck(ctx context.Context, client pb.AgentServiceClient) {
	resp, err := client.HealthCheck(ctx, &pb.HealthCheckRequest{})
	if err != nil {
		log.Fatalf("Health check failed: %v", err)
	}

	fmt.Printf("Agent Health Check:\n")
	fmt.Printf("  Healthy: %v\n", resp.Healthy)
	fmt.Printf("  Version: %s\n", resp.Version)
	fmt.Printf("  Runtime Status:\n")
	for runtime, status := range resp.RuntimeStatus {
		fmt.Printf("    %s: %s\n", runtime, status)
	}
}

func applyWorkload(ctx context.Context, client pb.AgentServiceClient, workloadID string) {
	if workloadID == "" {
		workloadID = "example-nginx"
	}

	// Example: Create an nginx container
	req := &pb.ApplyWorkloadRequest{
		Id:           workloadID,
		Type:         pb.WorkloadType_WORKLOAD_TYPE_CONTAINER,
		RevisionId:   "rev-1",
		DesiredState: pb.DesiredState_DESIRED_STATE_RUNNING,
		Spec: &pb.WorkloadSpec{
			Spec: &pb.WorkloadSpec_Container{
				Container: &pb.ContainerSpec{
					Image: "nginx:latest",
					Ports: []*pb.PortMapping{
						{
							HostPort:      8080,
							ContainerPort: 80,
							Protocol:      "tcp",
						},
					},
					Env: map[string]string{
						"NGINX_HOST": "localhost",
					},
					RestartPolicy: &pb.RestartPolicy{
						Policy:        "unless-stopped",
						MaxRetryCount: 0,
					},
				},
			},
		},
	}

	resp, err := client.ApplyWorkload(ctx, req)
	if err != nil {
		log.Fatalf("Failed to apply workload: %v", err)
	}

	fmt.Printf("Workload Applied:\n")
	fmt.Printf("  Applied: %v\n", resp.Applied)
	fmt.Printf("  Skipped: %v\n", resp.Skipped)
	fmt.Printf("  Message: %s\n", resp.Message)
	if resp.Status != nil {
		printStatus(resp.Status)
	}
}

func applyComposeWorkload(ctx context.Context, client pb.AgentServiceClient, workloadID string) {
	// Example docker-compose.yml
	composeYAML := `version: '3.8'
services:
  web:
    image: nginx:latest
    ports:
      - "8080:80"
  redis:
    image: redis:alpine
`

	encodedYAML := base64.StdEncoding.EncodeToString([]byte(composeYAML))

	req := &pb.ApplyWorkloadRequest{
		Id:           workloadID,
		Type:         pb.WorkloadType_WORKLOAD_TYPE_COMPOSE,
		RevisionId:   "rev-1",
		DesiredState: pb.DesiredState_DESIRED_STATE_RUNNING,
		Spec: &pb.WorkloadSpec{
			Spec: &pb.WorkloadSpec_Compose{
				Compose: &pb.ComposeSpec{
					ProjectName: workloadID,
					ComposeYaml: encodedYAML,
					Env: map[string]string{
						"COMPOSE_PROJECT_NAME": workloadID,
					},
				},
			},
		},
	}

	resp, err := client.ApplyWorkload(ctx, req)
	if err != nil {
		log.Fatalf("Failed to apply compose workload: %v", err)
	}

	fmt.Printf("Compose Workload Applied:\n")
	fmt.Printf("  Applied: %v\n", resp.Applied)
	fmt.Printf("  Message: %s\n", resp.Message)
}

func applyVMWorkload(ctx context.Context, client pb.AgentServiceClient, workloadID string) {
	if workloadID == "" {
		workloadID = "example-vm"
	}

	// Cloud-init script to configure the VM
	cloudInitScript := `#!/bin/bash
set -e
echo "Cloud-init starting at $(date)" > /tmp/cloud-init.log
# Update system
apt-get update
apt-get install -y curl wget openssh-server
# Configure hostname
hostnamectl set-hostname ` + workloadID + `
echo "Cloud-init completed at $(date)" >> /tmp/cloud-init.log
`

	// Example: Create a VM workload with ISO boot and cloud-init
	req := &pb.ApplyWorkloadRequest{
		Id:           workloadID,
		Type:         pb.WorkloadType_WORKLOAD_TYPE_VM,
		RevisionId:   "rev-1",
		DesiredState: pb.DesiredState_DESIRED_STATE_RUNNING,
		Spec: &pb.WorkloadSpec{
			Spec: &pb.WorkloadSpec_Vm{
				Vm: &pb.VMSpec{
					Name:      workloadID,
					Vcpus:     2,
					MemoryMb:  2048,
					CloudInit: cloudInitScript,
					Disks: []*pb.DiskConfig{
						// Boot disk
						{
							Path:   "/var/lib/libvirt/images/" + workloadID + ".qcow2",
							Device: "vda",
							Format: "qcow2",
							SizeGb: 20,
							Type:   "disk",
							Boot:   false,
						},
						// Installation ISO (optional - uncomment to use)
						{
							Path:   "/home/milx/Desktop/fedora-coreos-42.20250803.3.0-live-iso.x86_64.iso",
							Device: "hdc",
							Format: "raw",
							Type:   "cdrom",
							Boot:   true,
						},
					},
					Networks: []*pb.NetworkConfig{
						{
							Network:    "default",
							MacAddress: "52:54:00:12:34:56",
						},
					},
					Metadata: map[string]string{
						"environment": "test",
						"owner":       "demo",
						"created-by":  "test-client",
					},
				},
			},
		},
	}

	resp, err := client.ApplyWorkload(ctx, req)
	if err != nil {
		log.Fatalf("Failed to apply VM workload: %v", err)
	}

	fmt.Printf("VM Workload Applied:\n")
	fmt.Printf("  Applied: %v\n", resp.Applied)
	fmt.Printf("  Skipped: %v\n", resp.Skipped)
	fmt.Printf("  Message: %s\n", resp.Message)
	if resp.Status != nil {
		printStatus(resp.Status)
	}
}

func getStatus(ctx context.Context, client pb.AgentServiceClient, workloadID string) {
	if workloadID == "" {
		log.Fatal("Workload ID required for status check")
	}

	resp, err := client.GetWorkloadStatus(ctx, &pb.GetWorkloadStatusRequest{
		Id: workloadID,
	})
	if err != nil {
		log.Fatalf("Failed to get status: %v", err)
	}

	printStatus(resp.Status)
}

func listWorkloads(ctx context.Context, client pb.AgentServiceClient) {
	resp, err := client.ListWorkloads(ctx, &pb.ListWorkloadsRequest{})
	if err != nil {
		log.Fatalf("Failed to list workloads: %v", err)
	}

	fmt.Printf("Workloads (%d total):\n", len(resp.Workloads))
	for i, status := range resp.Workloads {
		fmt.Printf("\n[%d] %s\n", i+1, status.Id)
		printStatus(status)
	}
}

func deleteWorkload(ctx context.Context, client pb.AgentServiceClient, workloadID string) {
	if workloadID == "" {
		log.Fatal("Workload ID required for deletion")
	}

	resp, err := client.DeleteWorkload(ctx, &pb.DeleteWorkloadRequest{
		Id: workloadID,
	})
	if err != nil {
		log.Fatalf("Failed to delete workload: %v", err)
	}

	fmt.Printf("Delete Workload:\n")
	fmt.Printf("  Success: %v\n", resp.Success)
	fmt.Printf("  Message: %s\n", resp.Message)
}

func printStatus(status *pb.WorkloadStatus) {
	fmt.Printf("  ID: %s\n", status.Id)
	fmt.Printf("  Type: %s\n", status.Type)
	fmt.Printf("  Revision: %s\n", status.RevisionId)
	fmt.Printf("  Desired State: %s\n", status.DesiredState)
	fmt.Printf("  Actual State: %s\n", status.ActualState)
	fmt.Printf("  Message: %s\n", status.Message)
	fmt.Printf("  Created: %s\n", time.Unix(status.CreatedAt, 0).Format(time.RFC3339))
	fmt.Printf("  Updated: %s\n", time.Unix(status.UpdatedAt, 0).Format(time.RFC3339))
	if len(status.Metadata) > 0 {
		fmt.Printf("  Metadata:\n")
		for k, v := range status.Metadata {
			fmt.Printf("    %s: %s\n", k, v)
		}
	}
}
