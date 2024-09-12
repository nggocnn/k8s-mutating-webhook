package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// Default configuration values
var (
	veleroNamespace          string = "velero"    // Velero Namespace
	cronExpression           string = "@every 1h" // Velero Schedule - Cron expression define when to run the Backup
	csiSnapshotTimeout       string = "10m"       // csiSnapshotTimeout specifies the time used to wait for CSI VolumeSnapshot status turns to ReadyToUse during creation, before returning error as timeout. The default value is 10 minute.
	storageLocation          string = "default"   // Where Velero store the tarball and logs
	backupTTL                string = "720h0m0s"  // The amount of time before backups created on this schedule are eligible for garbage collection. If not specified, a default value of 30 days will be used.
	defaultVolumesToFsBackup bool   = true        // whether pod volume file system backup should be used for all volumes by default.
	backupSuffix             string = "backup"    // Backup sufix
	logFormat                string = "text"      // Log format (text or json)
	logLevel                 string = "info"      // Log level (debug, info, warn, error, fatal, panic)
)

func main() {
	setEnv() // Get and set environment variables

	// Set up HTTP handlers for the validation and health endpoints
	http.HandleFunc("/validate", ServerNamespaceBackup)
	http.HandleFunc("/health", ServerHealth)

	// Start the HTTPS server with TLS certificates
	cert := "/etc/admission-webhook/tls/tls.crt"
	key := "/etc/admission-webhook/tls/tls.key"
	logrus.Print("Listening on port 443...")
	logrus.Fatal(http.ListenAndServeTLS(":443", cert, key, nil))
}

// ServerNamespaceBackup handles the admission webhook requests (create/update/delete namespaces) to create/delete Velero schedules and backups.
func ServerNamespaceBackup(w http.ResponseWriter, r *http.Request) {
	logger := logrus.WithFields(logrus.Fields{"uri": r.RequestURI})

	// Parse the admission request
	admissionReview, err := parseRequest(*r)
	if err != nil {
		logger.WithFields(logrus.Fields{"error": err}).Error("Failed to parse request")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	oldNamespace := corev1.Namespace{}
	namespace := corev1.Namespace{}

	// Handle the operations: Create, Update, Delete
	switch admissionReview.Request.Operation {
	case admissionv1.Create:
		err := json.Unmarshal(admissionReview.Request.Object.Raw, &namespace)
		if err != nil {
			logger.WithFields(logrus.Fields{"error": err}).Error("Failed to parse namespace")
			http.Error(w, fmt.Sprintf("Could not parse namespace: %v", err), http.StatusBadRequest)
			return
		}
		logger.Info(fmt.Sprintf("Namespace %s created", namespace.Name))
	case admissionv1.Update:
		err := json.Unmarshal(admissionReview.Request.Object.Raw, &namespace)
		if err != nil {
			logger.WithFields(logrus.Fields{"error": err}).Error("Failed to parse namespace")
			http.Error(w, fmt.Sprintf("Could not parse namespace: %v", err), http.StatusBadRequest)
			return
		}
		logger.Info(fmt.Sprintf("Namespace %s updated", namespace.Name))
		err = json.Unmarshal(admissionReview.Request.OldObject.Raw, &oldNamespace)
		if err != nil {
			logger.WithFields(logrus.Fields{"error": err}).Error("Failed to parse old namespace")
			http.Error(w, fmt.Sprintf("Could not parse old namespace: %v", err), http.StatusBadRequest)
			return
		}
	case admissionv1.Delete:
		err = json.Unmarshal(admissionReview.Request.OldObject.Raw, &oldNamespace)
		if err != nil {
			logger.WithFields(logrus.Fields{"error": err}).Error("Failed to parse old namespace")
			http.Error(w, fmt.Sprintf("Could not parse old namespace: %v", err), http.StatusBadRequest)
			return
		}
		logger.Info(fmt.Sprintf("Namespace %s deleted", oldNamespace.Name))
	default:
		logger.Info("Unknown operation")
		return
	}

	// Get in-cluster Kubernetes configuration
	config, err := rest.InClusterConfig()
	if err != nil {
		logger.WithFields(logrus.Fields{"error": err}).Error("Failed to get in-cluster config")
		http.Error(w, fmt.Sprintf("Could not get in-cluster config: %v", err), http.StatusInternalServerError)
		return
	}

	// Create a dynamic Kubernetes client
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		logger.WithFields(logrus.Fields{"error": err}).Error("Failed to create client")
		http.Error(w, fmt.Sprintf("Could not create client: %v", err), http.StatusInternalServerError)
		return
	}

	// Extract labels to determine whether to create, update, or delete Kubevela target
	targetName, targetKey := namespace.Labels["namespace.oam.dev/target"]
	runtime, runtimeKey := namespace.Labels["usage.oam.dev/runtime"]
	oldTargetName, oldTargetKey := oldNamespace.Labels["namespace.oam.dev/target"]
	oldRuntime, oldRuntimeKey := oldNamespace.Labels["usage.oam.dev/runtime"]

	switch admissionReview.Request.Operation {
	case admissionv1.Create:
		// Create Velero schedule and backup if the namespace is restored
		if targetKey && targetName != "" && runtimeKey && runtime == "target" {
			createVeleroSchedule(*r, dynamicClient, namespace.Name, logger)
			createVeleroBackup(*r, dynamicClient, namespace.Name, logger)
		}
	case admissionv1.Update:
		// Create Velero schedule and backup if the namespace is updated to a target, or delete if no longer a target
		if targetKey && targetName != "" && runtimeKey && runtime == "target" && (!oldTargetKey || oldTargetName == "" || !oldRuntimeKey || oldRuntime == "") {
			createVeleroSchedule(*r, dynamicClient, namespace.Name, logger)
			createVeleroBackup(*r, dynamicClient, namespace.Name, logger)
		} else if (!targetKey || targetName == "" || !runtimeKey || runtime != "target") && oldTargetKey && oldTargetName != "" && oldRuntimeKey && oldRuntime == "target" {
			deleteVeleroSchedule(*r, dynamicClient, namespace.Name, logger)
		}

	case admissionv1.Delete:
		// Delete Velero schedule if the namespace is deleted and was a target
		if oldTargetKey && oldTargetName != "" && oldRuntimeKey && oldRuntime == "target" {
			deleteVeleroSchedule(*r, dynamicClient, oldNamespace.Name, logger)
		}
	}

	// Respond to the admission request
	response := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Response: &admissionv1.AdmissionResponse{
			UID:     admissionReview.Request.UID,
			Allowed: true,
		},
	}

	// Marshal the response into JSON and write it to the response writer
	respBytes, err := json.Marshal(response)
	if err != nil {
		logger.WithFields(logrus.Fields{"error": err}).Error("Failed to marshal response")
		http.Error(w, fmt.Sprintf("Could not marshal response: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(respBytes)
}

// createVeleroSchedule creates a Velero schedule for backing up the given namespace.
func createVeleroSchedule(r http.Request, client dynamic.Interface, namespaceName string, logger *logrus.Entry) {
	scheduleName := fmt.Sprintf("%s-backup", namespaceName)

	veleroScheduleResource := schema.GroupVersionResource{
		Group:    "velero.io",
		Version:  "v1",
		Resource: "schedules",
	}

	// Check if the schedule already exists
	_, err := client.Resource(veleroScheduleResource).Namespace(veleroNamespace).Get(r.Context(), scheduleName, metav1.GetOptions{})
	if err != nil {
		logger.Info(fmt.Sprintf("%v", err))
	}

	// Define the Velero schedule object
	veleroSchedule := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "velero.io/v1",
			"kind":       "Schedule",
			"metadata": map[string]interface{}{
				"name":      scheduleName,
				"namespace": veleroNamespace,
			},
			"spec": map[string]interface{}{
				"schedule":                   cronExpression,
				"useOwnerReferencesInBackup": false,
				"template": map[string]interface{}{
					"csiSnapshotTimeout":       csiSnapshotTimeout,
					"includedNamespaces":       []string{namespaceName},
					"storageLocation":          storageLocation,
					"ttl":                      backupTTL,
					"defaultVolumesToFsBackup": defaultVolumesToFsBackup,
				},
			},
		},
	}

	// Create the Velero schedule
	logger.Info(fmt.Sprintf("Creating Velero schedule %s", scheduleName))
	_, err = client.Resource(veleroScheduleResource).Namespace(veleroNamespace).Create(r.Context(), veleroSchedule, metav1.CreateOptions{})
	if err != nil {
		logger.WithFields(logrus.Fields{"error": err}).Error("Failed to create Velero schedule")
	} else {
		logger.Info("Velero schedule created successfully")
	}
}

// createVeleroBackup creates an instant Velero backup for the given namespace.
func createVeleroBackup(r http.Request, client dynamic.Interface, namespaceName string, logger *logrus.Entry) {
	scheduleName := fmt.Sprintf("%s-backup", namespaceName)
	backupName := fmt.Sprintf("%s-%s", scheduleName, time.Now().Format("20060102150405"))

	veleroBackupResource := schema.GroupVersionResource{
		Group:    "velero.io",
		Version:  "v1",
		Resource: "backups",
	}

	// Define the Velero backup object
	veleroBackup := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "velero.io/v1",
			"kind":       "Backup",
			"metadata": map[string]interface{}{
				"name":      backupName,
				"namespace": veleroNamespace,
			},
			"spec": map[string]interface{}{
				"csiSnapshotTimeout":       csiSnapshotTimeout,
				"includedNamespaces":       []string{namespaceName},
				"storageLocation":          storageLocation,
				"ttl":                      backupTTL,
				"defaultVolumesToFsBackup": defaultVolumesToFsBackup,
			},
		},
	}

	// Create the Velero backup
	logger.Info(fmt.Sprintf("Creating Velero backup %s", backupName))
	_, err := client.Resource(veleroBackupResource).Namespace(veleroNamespace).Create(r.Context(), veleroBackup, metav1.CreateOptions{})
	if err != nil {
		logger.WithFields(logrus.Fields{"error": err}).Error("Failed to create Velero backup")
	} else {
		logger.Info("Velero backup created successfully")
	}
}

// deleteVeleroSchedule deletes a Velero schedule associated with the given namespace.
func deleteVeleroSchedule(r http.Request, client dynamic.Interface, namespaceName string, logger *logrus.Entry) {
	scheduleName := fmt.Sprintf("%s-backup", namespaceName)
	veleroScheduleResource := schema.GroupVersionResource{
		Group:    "velero.io",
		Version:  "v1",
		Resource: "schedules",
	}

	// Attempt to delete the Velero schedule
	logger.Info(fmt.Sprintf("Deleting Velero schedule %s", scheduleName))
	err := client.Resource(veleroScheduleResource).Namespace(veleroNamespace).Delete(r.Context(), scheduleName, metav1.DeleteOptions{})
	if err != nil {
		logger.WithFields(logrus.Fields{"error": err}).Error("Failed to delete Velero schedule")
	} else {
		logger.Info("Velero schedule deleted successfully")
	}
}

// parseRequest parses the incoming admission webhook request into an AdmissionReview object.
func parseRequest(r http.Request) (*admissionv1.AdmissionReview, error) {
	if r.Header.Get("Content-Type") != "application/json" {
		return nil, fmt.Errorf("Content-Type: %q should be %q", r.Header.Get("Content-Type"), "application/json")
	}

	bodybuf := new(bytes.Buffer)
	bodybuf.ReadFrom(r.Body)
	body := bodybuf.Bytes()

	if len(body) == 0 {
		return nil, fmt.Errorf("admission request body is empty")
	}

	var admissionReview admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &admissionReview); err != nil {
		return nil, fmt.Errorf("failed to parse admission request: %v", err)
	}

	if admissionReview.Request == nil {
		return nil, fmt.Errorf("admission request is empty")
	}

	return &admissionReview, nil
}

// setEnv reads environment variables and overrides the default configuration values if they are set.
func setEnv() {
	logger := logrus.WithFields(logrus.Fields{})

	// Load environment variables and assign to global variables
	veleroNamespace = getEnv("VELERO_NAMESPACE", veleroNamespace)

	cronExpression = getEnv("CRON_EXPRESSION", cronExpression)

	csiSnapshotTimeout = getEnv("CSI_SNAPSHOT_TIMEOUT", csiSnapshotTimeout)

	storageLocation = getEnv("STORAGE_LOCATION", storageLocation)

	backupTTL = getEnv("BACKUP_TTL", backupTTL)

	defaultVolumesToFsBackupEnv := os.Getenv("DEFAULT_VOLUMES_TO_FS_BACKUP")
	if defaultVolumesToFsBackupEnv == "" || strings.ToLower(defaultVolumesToFsBackupEnv) == "true" {
		defaultVolumesToFsBackup = true
	} else {
		defaultVolumesToFsBackup = false
	}

	backupSuffix = getEnv("BACKUP_SUFFIX", backupSuffix)

	logFormat = getEnv("LOG_FORMAT", "text")

	logLevel = getEnv("LOG_LEVEL", "")

	logger.WithFields(logrus.Fields{
		"veleroNamespace":          veleroNamespace,
		"cronExpression":           cronExpression,
		"csiSnapshotTimeout":       csiSnapshotTimeout,
		"storageLocation":          storageLocation,
		"backupTTL":                backupTTL,
		"defaultVolumesToFsBackup": defaultVolumesToFsBackup,
		"backupSuffix":             backupSuffix,
		"logFormat":                logFormat,
		"logLevel":                 logLevel,
	}).Info("Set environment variables")

	if logFormat == "json" {
		logrus.SetFormatter(&logrus.JSONFormatter{})
	} else {
		logrus.SetFormatter(&logrus.TextFormatter{})
	}

	logrus.SetLevel(logrus.DebugLevel)

	if logLevel != "" {
		level, err := logrus.ParseLevel(logLevel)
		if err != nil {
			logger.WithFields(logrus.Fields{"error": err}).Error("Failed to parse log level")
		}
		logrus.SetLevel(level)
	}
}

func getEnv(key string, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

// ServerHealth returns a 200 OK response to indicate that the webhook server is healthy.
func ServerHealth(w http.ResponseWriter, r *http.Request) {
	logrus.WithFields(logrus.Fields{"uri": r.RequestURI}).Debug("Healthy")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
