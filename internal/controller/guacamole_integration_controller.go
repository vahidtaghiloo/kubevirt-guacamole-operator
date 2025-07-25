/*
Copyright 2025.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use thle except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	corev1 "k8s.io/api/core/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

const (
	// Finalizer to ensure proper cleanup
	VMWatcherFinalizer = "vm-watcher.setofangdar.polito.it/finalizer"
	// Annotation to track if we've already processed this VM
	ProcessedAnnotation = "vm-watcher.setofangdar.polito.it/processed"
	// Annotation to track the last known status
	LastStatusAnnotation = "vm-watcher.setofangdar.polito.it/last-status"
	// Default retry delay
	DefaultRetryDelay = 2 * time.Minute
	// Maximum retry attempts
	MaxRetryAttempts = 3
)

// VirtualMachineReconciler reconciles KubeVirt VirtualMachine objects
type VirtualMachineReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	GuacamoleBaseURL  string // Base URL of Guacamole (e.g., https://guacamole.example.com)
	GuacamoleUsername string // Guacamole admin username
	GuacamolePassword string // Guacamole admin password
	HTTPClient        *http.Client
}

// GuacamoleAuthResponse represents the authentication response from Guacamole
type GuacamoleAuthResponse struct {
	AuthToken            string   `json:"authToken"`
	Username             string   `json:"username"`
	DataSource           string   `json:"dataSource"`
	AvailableDataSources []string `json:"availableDataSources"`
}

// GuacamoleConnection represents a Guacamole connection configuration
type GuacamoleConnection struct {
	ParentIdentifier string            `json:"parentIdentifier"`
	Name             string            `json:"name"`
	Protocol         string            `json:"protocol"`
	Parameters       map[string]string `json:"parameters"`
	Attributes       map[string]string `json:"attributes"`
}

// GuacamoleConnectionResponse represents the response when creating a connection
type GuacamoleConnectionResponse struct {
	Identifier       string            `json:"identifier"`
	ParentIdentifier string            `json:"parentIdentifier"`
	Name             string            `json:"name"`
	Protocol         string            `json:"protocol"`
	Parameters       map[string]string `json:"parameters"`
	Attributes       map[string]string `json:"attributes"`
}

// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines/status,verbs=get
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch

func (r *VirtualMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the VirtualMachine instance
	var vm kubevirtv1.VirtualMachine
	if err := r.Get(ctx, req.NamespacedName, &vm); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("VM not found, likely deleted", "name", req.Name, "namespace", req.Namespace)
			// VM is already deleted, nothing to do
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if vm.DeletionTimestamp != nil {
		logger.Info("VM is being deleted, handling cleanup", "name", vm.Name, "namespace", vm.Namespace)
		return r.handleDeletion(ctx, &vm)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(&vm, VMWatcherFinalizer) {
		controllerutil.AddFinalizer(&vm, VMWatcherFinalizer)
		if err := r.Update(ctx, &vm); err != nil {
			logger.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Check if this is a new VM that we haven't processed yet
	isNewVM := vm.Annotations[ProcessedAnnotation] != "true"

	// Check if status has changed
	currentStatus := string(vm.Status.PrintableStatus)
	lastStatus := vm.Annotations[LastStatusAnnotation]
	statusChanged := lastStatus != "" && lastStatus != currentStatus

	if isNewVM {
		logger.Info("New VM detected", "name", vm.Name, "namespace", vm.Namespace)

		// Wait for VM to be running before creating Guacamole connection
		if vm.Status.PrintableStatus != kubevirtv1.VirtualMachineStatusRunning {
			logger.Info("VM not yet running, waiting", "name", vm.Name, "status", vm.Status.PrintableStatus)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		// Create Guacamole connection
		// ConnectionID is Guacamole's unique identifier for the created connection
		_, err := r.createGuacamoleConnection(ctx, &vm)
		if err != nil {
			logger.Error(err, "Failed to create Guacamole connection")
			// Instead of controlled controller-runtime's exponential backoff, use retry timing for external API failures
			return ctrl.Result{RequeueAfter: DefaultRetryDelay}, nil
		}

		// Mark as processed
		if vm.Annotations == nil {
			vm.Annotations = make(map[string]string)
		}
		vm.Annotations[ProcessedAnnotation] = "true"
		vm.Annotations[LastStatusAnnotation] = currentStatus
		if err := r.Update(ctx, &vm); err != nil {
			logger.Error(err, "Failed to update processed annotation")
			return ctrl.Result{}, err
		}

		logger.Info("Successfully created Guacamole connection",
			"vm", vm.Name,
			"namespace", vm.Namespace)
	} else if statusChanged {
		logger.Info("VM status changed", "name", vm.Name, "old_status", lastStatus, "new_status", currentStatus)

		// Handle status changes
		if currentStatus == string(kubevirtv1.VirtualMachineStatusStopped) {
			// VM stopped, connection may need to be disabled
			logger.Info("VM stopped, connection may need to be disabled", "vm", vm.Name)
		} else if currentStatus == string(kubevirtv1.VirtualMachineStatusRunning) {
			// VM restarted, update connection if needed
			logger.Info("VM restarted, connection may need to be updated", "vm", vm.Name)
		}

		// Update last status
		if vm.Annotations == nil {
			vm.Annotations = make(map[string]string)
		}
		vm.Annotations[LastStatusAnnotation] = currentStatus
		if err := r.Update(ctx, &vm); err != nil {
			logger.Error(err, "Failed to update last status annotation")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *VirtualMachineReconciler) handleDeletion(ctx context.Context, vm *kubevirtv1.VirtualMachine) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.Info("Handling VM deletion", "name", vm.Name, "namespace", vm.Namespace)
	
	// Always try to delete by name first (most reliable approach)
	connectionName := fmt.Sprintf("%s-%s", vm.Namespace, vm.Name)
	logger.Info("Deleting Guacamole connection by name", "connection_name", connectionName)
	
	if err := r.deleteGuacamoleConnectionByName(ctx, connectionName); err != nil {
		logger.Error(err, "Failed to delete Guacamole connection", "connection_name", connectionName)
		// Don't fail the deletion - log and continue
	}

	// Remove our finalizer
	if controllerutil.ContainsFinalizer(vm, VMWatcherFinalizer) {
		controllerutil.RemoveFinalizer(vm, VMWatcherFinalizer)
		if err := r.Update(ctx, vm); err != nil {
			logger.Error(err, "Failed to remove finalizer")
			return ctrl.Result{}, err
		}
		logger.Info("Successfully removed finalizer", "name", vm.Name, "namespace", vm.Namespace)
	}

	logger.Info("Successfully handled VM deletion", "name", vm.Name, "namespace", vm.Namespace)
	return ctrl.Result{}, nil
}

// authenticateWithGuacamole gets an authentication token from Guacamole
func (r *VirtualMachineReconciler) authenticateWithGuacamole(ctx context.Context) (*GuacamoleAuthResponse, error) {
	if r.GuacamoleBaseURL == "" {
		return nil, fmt.Errorf("guacamole base url not configured")
	}

	authURL := fmt.Sprintf("%s/api/tokens", strings.TrimSuffix(r.GuacamoleBaseURL, "/"))

	// Prepare form data
	data := url.Values{}
	data.Set("username", r.GuacamoleUsername)
	data.Set("password", r.GuacamolePassword)

	req, err := http.NewRequestWithContext(ctx, "POST", authURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create auth request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := r.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to authenticate with Guacamole: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authentication failed with status %d", resp.StatusCode)
	}

	var authResp GuacamoleAuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&authResp); err != nil {
		return nil, fmt.Errorf("failed to decode auth response: %w", err)
	}

	return &authResp, nil
}

// createGuacamoleConnection creates a new connection in Guacamole for the VM
func (r *VirtualMachineReconciler) createGuacamoleConnection(ctx context.Context, vm *kubevirtv1.VirtualMachine) (string, error) {
	logger := log.FromContext(ctx)

	// Authenticate with Guacamole
	authResp, err := r.authenticateWithGuacamole(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to authenticate: %w", err)
	}

	// Get VM connection details
	connection, err := r.buildGuacamoleConnection(ctx, vm)
	if err != nil {
		return "", fmt.Errorf("failed to build connection config: %w", err)
	}

	// Create connection via API
	createURL := fmt.Sprintf("%s/api/session/data/%s/connections?token=%s",
		strings.TrimSuffix(r.GuacamoleBaseURL, "/"),
		authResp.DataSource,
		authResp.AuthToken)

	body, err := json.Marshal(connection)
	if err != nil {
		return "", fmt.Errorf("failed to marshal connection: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", createURL, bytes.NewBuffer(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	client := r.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to create connection: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("connection creation failed with status %d", resp.StatusCode)
	}

	var connResp GuacamoleConnectionResponse
	if err := json.NewDecoder(resp.Body).Decode(&connResp); err != nil {
		return "", fmt.Errorf("failed to decode connection response: %w", err)
	}

	logger.Info("Successfully created Guacamole connection",
		"vm", vm.Name,
		"connection_id", connResp.Identifier,
		"protocol", connResp.Protocol)

	return connResp.Identifier, nil
}

// buildGuacamoleConnection builds the connection configuration for Guacamole
func (r *VirtualMachineReconciler) buildGuacamoleConnection(ctx context.Context, vm *kubevirtv1.VirtualMachine) (*GuacamoleConnection, error) {
	logger := log.FromContext(ctx)

	// Default to RDP protocol
	protocol := "rdp"
	port := "3389"

	// Check for custom protocol in annotations
	if vm.Annotations != nil {
		if customProtocol, exists := vm.Annotations["vm-watcher.setofangdar.polito.it/protocol"]; exists {
			normalizedProtocol := strings.ToLower(customProtocol)
			// Only allow RDP and VNC protocols
			if normalizedProtocol == "rdp" || normalizedProtocol == "vnc" {
				protocol = normalizedProtocol
			} else {
				logger.Info("Unsupported protocol specified, defaulting to RDP",
					"vm", vm.Name,
					"requestedProtocol", customProtocol,
					"supportedProtocols", "rdp, vnc")
			}
		}
		if customPort, exists := vm.Annotations["vm-watcher.setofangdar.polito.it/port"]; exists {
			port = customPort
		}
	}

	// Set default ports based on protocol
	switch protocol {
	case "vnc":
		if port == "3389" { // If still default RDP port
			port = "5900"
		}
	case "rdp":
		if port == "5900" { // If still default VNC port
			port = "3389"
		}
	}

	// Get VM IP address
	hostname, err := r.getVMHostname(ctx, vm)
	if err != nil {
		return nil, fmt.Errorf("failed to get VM hostname: %w", err)
	}

	// Build connection name
	connectionName := fmt.Sprintf("%s-%s", vm.Namespace, vm.Name)

	// Build parameters based on protocol
	parameters := make(map[string]string)
	parameters["hostname"] = hostname
	parameters["port"] = port

	// Add protocol-specific parameters
	switch protocol {
	case "vnc":
		parameters["color-depth"] = "24"
		parameters["cursor"] = "remote"
		parameters["read-only"] = "false"
		parameters["swap-red-blue"] = "false"
		parameters["disable-copy"] = "false"
		parameters["disable-paste"] = "false"
		parameters["enable-audio"] = "false"

		// Add VNC password if provided
		if vm.Annotations != nil {
			if password, exists := vm.Annotations["vm-watcher.setofangdar.polito.it/password"]; exists {
				parameters["password"] = password
			}
		}

	case "rdp":
		parameters["security"] = "any"
		parameters["ignore-cert"] = "true"
		parameters["disable-auth"] = "false"
		parameters["resize-method"] = "reconnect"
		parameters["console-audio"] = "false"
		parameters["disable-audio"] = "false"
		parameters["enable-wallpaper"] = "false"
		parameters["enable-theming"] = "false"
		parameters["enable-font-smoothing"] = "false"

		// Add RDP credentials if provided
		if vm.Annotations != nil {
			if username, exists := vm.Annotations["vm-watcher.setofangdar.polito.it/username"]; exists {
				parameters["username"] = username
			}
			if password, exists := vm.Annotations["vm-watcher.setofangdar.polito.it/password"]; exists {
				parameters["password"] = password
			}
			if domain, exists := vm.Annotations["vm-watcher.setofangdar.polito.it/domain"]; exists {
				parameters["domain"] = domain
			}
		}

	default:
		// This should not happen due to validation above, but handle gracefully
		return nil, fmt.Errorf("unsupported protocol '%s', only 'rdp' and 'vnc' are supported", protocol)
	}

	// Set empty values for unused parameters (Guacamole expects all parameters)
	emptyParams := []string{
		"recording-path", "recording-name", "recording-exclude-output",
		"recording-exclude-mouse", "recording-include-keys", "create-recording-path",
		"dest-host", "dest-port",
	}
	for _, param := range emptyParams {
		if _, exists := parameters[param]; !exists {
			parameters[param] = ""
		}
	}

	// Build attributes
	attributes := map[string]string{
		"max-connections":          "",
		"max-connections-per-user": "",
		"weight":                   "",
		"failover-only":            "",
		"guacd-port":               "",
		"guacd-encryption":         "",
		"guacd-hostname":           "",
	}

	connection := &GuacamoleConnection{
		ParentIdentifier: "ROOT",
		Name:             connectionName,
		Protocol:         protocol,
		Parameters:       parameters,
		Attributes:       attributes,
	}

	logger.Info("Built Guacamole connection config",
		"vm", vm.Name,
		"protocol", protocol,
		"hostname", hostname,
		"port", port)

	return connection, nil
}

// getVMHostname extracts the hostname/IP for the VM
func (r *VirtualMachineReconciler) getVMHostname(ctx context.Context, vm *kubevirtv1.VirtualMachine) (string, error) {
	// Try to get the VMI to extract IP address
	var vmi kubevirtv1.VirtualMachineInstance
	vmiKey := client.ObjectKey{
		Namespace: vm.Namespace,
		Name:      vm.Name,
	}

	if err := r.Get(ctx, vmiKey, &vmi); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return "", fmt.Errorf("failed to get VMI: %w", err)
		}
		// VMI not found, use VM name as hostname
		return vm.Name, nil
	}

	// Extract IP address from VMI status
	if len(vmi.Status.Interfaces) > 0 {
		for _, iface := range vmi.Status.Interfaces {
			if iface.IP != "" {
				return iface.IP, nil
			}
		}
	}

	// If no IP found, try to find a service that might expose this VM
	var services corev1.ServiceList
	if err := r.List(ctx, &services, client.InNamespace(vm.Namespace)); err == nil {
		for _, svc := range services.Items {
			if svc.Spec.Selector != nil {
				// Check if service selector matches VM labels
				matches := true
				for key, value := range svc.Spec.Selector {
					if vm.Labels[key] != value {
						matches = false
						break
					}
				}
				if matches {
					// Use service name as hostname
					return fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, svc.Namespace), nil
				}
			}
		}
	}

	// Fallback to VM name
	return vm.Name, nil
}

// deleteGuacamoleConnection deletes the Guacamole connection when VM is deleted
func (r *VirtualMachineReconciler) deleteGuacamoleConnection(ctx context.Context, connectionID string) error {
	logger := log.FromContext(ctx)

	if connectionID == "" {
		logger.Info("No Guacamole connection ID provided, skipping deletion")
		return nil
	}

	logger.Info("Deleting Guacamole connection", "connection_id", connectionID)

	// Authenticate with Guacamole
	authResp, err := r.authenticateWithGuacamole(ctx)
	if err != nil {
		return fmt.Errorf("failed to authenticate: %w", err)
	}

	// Delete connection via API
	deleteURL := fmt.Sprintf("%s/api/session/data/%s/connections/%s?token=%s",
		strings.TrimSuffix(r.GuacamoleBaseURL, "/"),
		authResp.DataSource,
		connectionID,
		authResp.AuthToken)

	logger.Info("Making DELETE request to Guacamole", "url", deleteURL)

	req, err := http.NewRequestWithContext(ctx, "DELETE", deleteURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create delete request: %w", err)
	}

	client := r.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to delete connection: %w", err)
	}
	defer resp.Body.Close()

	logger.Info("Received response from Guacamole", "status_code", resp.StatusCode)

	// Handle different status codes
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		logger.Info("Successfully deleted Guacamole connection", "connection_id", connectionID)
		return nil
	case http.StatusNotFound:
		logger.Info("Guacamole connection not found (already deleted?)", "connection_id", connectionID)
		return nil // Consider this a success since the connection is gone
	default:
		// Read response body for error details
		body := make([]byte, 1024)
		n, _ := resp.Body.Read(body)
		errorBody := string(body[:n])
		logger.Error(fmt.Errorf("connection deletion failed"), "status_code", resp.StatusCode, "response_body", errorBody)
		return fmt.Errorf("connection deletion failed with status %d: %s", resp.StatusCode, errorBody)
	}
}

// deleteGuacamoleConnectionByName deletes a Guacamole connection by searching for it by name
func (r *VirtualMachineReconciler) deleteGuacamoleConnectionByName(ctx context.Context, connectionName string) error {
	logger := log.FromContext(ctx)

	// First, authenticate with Guacamole
	authResp, err := r.authenticateWithGuacamole(ctx)
	if err != nil {
		return fmt.Errorf("failed to authenticate: %w", err)
	}

	// Get all connections to find the one with matching name
	connectionsURL := fmt.Sprintf("%s/api/session/data/%s/connections?token=%s",
		strings.TrimSuffix(r.GuacamoleBaseURL, "/"),
		authResp.DataSource,
		authResp.AuthToken)

	req, err := http.NewRequestWithContext(ctx, "GET", connectionsURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create connections request: %w", err)
	}

	client := r.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to get connections: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to get connections with status %d", resp.StatusCode)
	}

	// Parse the response to find connections with matching name
	var connections map[string]GuacamoleConnectionResponse
	if err := json.NewDecoder(resp.Body).Decode(&connections); err != nil {
		return fmt.Errorf("failed to decode connections response: %w", err)
	}

	// Find and delete connections with matching name
	var deletedAny bool
	for identifier, connection := range connections {
		if connection.Name == connectionName {
			logger.Info("Found matching connection to delete", "connection_id", identifier, "connection_name", connectionName)
			if err := r.deleteGuacamoleConnection(ctx, identifier); err != nil {
				logger.Error(err, "Failed to delete connection", "connection_id", identifier)
			} else {
				deletedAny = true
				logger.Info("Successfully deleted connection", "connection_id", identifier)
			}
		}
	}

	if !deletedAny {
		logger.Info("No matching connections found to delete", "connection_name", connectionName)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *VirtualMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Create a predicate to filter events we care about
	vmPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			// Always process create events
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			// Process update events only if annotations changed or status changed
			oldVM := e.ObjectOld.(*kubevirtv1.VirtualMachine)
			newVM := e.ObjectNew.(*kubevirtv1.VirtualMachine)

			// Process if the processed annotation is missing, status changed, or generation changed
			// Also process if deletion timestamp is set
			return oldVM.Annotations[ProcessedAnnotation] != newVM.Annotations[ProcessedAnnotation] ||
				oldVM.Status.PrintableStatus != newVM.Status.PrintableStatus ||
				oldVM.Generation != newVM.Generation ||
				(oldVM.DeletionTimestamp == nil && newVM.DeletionTimestamp != nil)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			// Always process delete events - this is critical for cleanup
			vm := e.Object.(*kubevirtv1.VirtualMachine)
			log.Log.Info("VM deletion event received", "name", vm.Name, "namespace", vm.Namespace)
			return true
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&kubevirtv1.VirtualMachine{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 2, // Allow some concurrency but not too much
		}).
		WithEventFilter(vmPredicate).
		Named("kubevirt-vm-watcher").
		Complete(r)
}
