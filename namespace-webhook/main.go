package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
    "time"
	"string"

	"github.com/sirupsen/logrus"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/runtime/schema"
    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var cronExpression string = "@every 1h" // Velero Schedule - Cron expression define when to run the Backup
var	csiSnapshotTimeout string = "10m" // csiSnapshotTimeout specifies the time used to wait for CSI VolumeSnapshot status turns to ReadyToUse during creation, before returning error as timeout. The default value is 10 minute.
var	storageLocation string = "default" // Where Velero store the tarball and logs
var	ttl string = "720h0m0s" // The amount of time before backups created on this schedule are eligible for garbage collection. If not specified, a default value of 30 days will be used.
var	defaultVolumesToFsBackup bool // whether pod volume file system backup should be used for all volumes by default.
var backupSufix string = "backup" // Backup sufix
var	logFormat string = "text" // Log format (text or json)
var	logLevel string = "" // Log level (debug, info, warn, error, fatal, panic)

func main() {
	setEnv()

	http.HandleFunc("/validate", ServerNamespaceBackup)
	http.HandleFunc("/health", ServerHealth)

	cert := "/etc/admission-webhook/tls/tls.crt"
	key := "/etc/admission-webhook/tls/tls.key"
	logrus.Print("Listening on port 443...")
	logrus.Fatal(http.ListenAndServeTLS(":443", cert, key, nil))
}

func ServerNamespaceBackup(w http.ResponseWriter, r *http.Request) {
	logger := logrus.WithFields(logrus.Fields{"uri": r.RequestURI})

	admissionReview, err := parseRequest(*r)
	if err != nil {
		logger.WithFields(logrus.Fields{"error": err}).Error("Failed to parse request")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

    oldNamespace := corev1.Namespace{}
	namespace := corev1.Namespace{}
	
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

	config, err := rest.InClusterConfig()
	if err != nil {
		logger.WithFields(logrus.Fields{"error": err}).Error("failed to get in-cluster config")
		http.Error(w, fmt.Sprintf("Could not get in-cluster config: %v", err), http.StatusInternalServerError)
		return
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		logger.WithFields(logrus.Fields{"error": err}).Error("failed to create client")
		http.Error(w, fmt.Sprintf("Could not create client: %v", err), http.StatusInternalServerError)
		return
	}

    targetName, targetKey := namespace.Labels["namespace.oam.dev/target"]
	runtime, runtimeKey := namespace.Labels["usage.oam.dev/runtime"]
	oldTargetName, oldTargetKey := oldNamespace.Labels["namespace.oam.dev/target"]
	oldRuntime, oldRuntimeKey := oldNamespace.Labels["usage.oam.dev/runtime"]

	switch admissionReview.Request.Operation {
		case admissionv1.Create:
			if targetKey && targetName != "" && runtimeKey && runtime == "target" {
				createVeleroSchedule(*r, dynamicClient, namespace.Name, logger)
				createVeleroBackup(*r, dynamicClient,  namespace.Name, logger)
			}
        case admissionv1.Update:
            if targetKey && targetName != "" && runtimeKey && runtime == "target" && (!oldTargetKey || oldTargetName == "" || !oldRuntimeKey || oldRuntime == "") {
                createVeleroSchedule(*r, dynamicClient, namespace.Name, logger)
                createVeleroBackup(*r, dynamicClient,  namespace.Name, logger)
            } else if (!targetKey || targetName == "" || !runtimeKey || runtime != "target") && oldTargetKey && oldTargetName != "" && oldRuntimeKey && oldRuntime == "target" {
                deleteVeleroSchedule(*r, dynamicClient, namespace.Name, logger)
            }
            
        case admissionv1.Delete:
            if oldTargetKey && oldTargetName != "" && oldRuntimeKey && oldRuntime == "target" {
                deleteVeleroSchedule(*r, dynamicClient, oldNamespace.Name, logger)
            }
    }

    response := admissionv1.AdmissionReview{
		Response: &admissionv1.AdmissionResponse{
			Allowed: true,
			UID: admissionReview.Request.UID,
		},
	}

	respBytes, err := json.Marshal(response)
	if err != nil {
		logger.WithFields(logrus.Fields{"error": err}).Error("Failed to marshal response")
		http.Error(w, fmt.Sprintf("Could not marshal response: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(respBytes)
}

func createVeleroSchedule(r http.Request, client dynamic.Interface, namespaceName string, logger *logrus.Entry) {
	scheduleName := fmt.Sprintf("%s-backup", namespaceName)
    
    veleroScheduleResource := schema.GroupVersionResource{
		Group: "velero.io",
		Version: "v1",
		Resource: "schedules",
	}

	_, err := client.Resource(veleroScheduleResource).Namespace("velero").Get(r.Context(), scheduleName, metav1.GetOptions{})
	if err != nil {
        logger.Info(fmt.Sprintf("%v", err))
	}

	veleroSchedule := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "velero.io/v1",
			"kind": "Schedule",
			"metadata": map[string]interface{}{
				"name": scheduleName,
				"namespace": "velero",
			},
			"spec": map[string]interface{}{
				"schedule": cronExpression,
				"useOwnerReferencesInBackup": false,
				"template": map[string]interface{}{
					"csiSnapshotTimeout": csiSnapshotTimeout,
					"includedNamespaces": []string{namespaceName},
					"storageLocation": storageLocation,
					"ttl": backupTTL,
					"defaultVolumesToFsBackup": defaultVolumesToFsBackup,
				},
			},
		},
	}

	logger.Info(fmt.Sprintf("Creating Velero schedule %s", scheduleName))
	_, err = client.Resource(veleroScheduleResource).Namespace("velero").Create(r.Context(), veleroSchedule, metav1.CreateOptions{})
	if err != nil {
        logger.Info(fmt.Sprintf("%v", err))
	}
}

func createVeleroBackup(r http.Request, client dynamic.Interface, namespaceName string, logger *logrus.Entry) {
	scheduleName := fmt.Sprintf("%s-backup", namespaceName)
    backupName := fmt.Sprintf("%s-%s", scheduleName, time.Now().Format("20060102150405"))

    veleroBackupResource := schema.GroupVersionResource{
		Group: "velero.io",
		Version: "v1",
		Resource: "backups",
	}

	veleroBackup := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "velero.io/v1",
			"kind": "Backup",
			"metadata": map[string]interface{}{
				"name": backupName,
				"namespace": "velero",
			},
			"spec": map[string]interface{}{
				"csiSnapshotTimeout": csiSnapshotTimeout,
				"includedNamespaces": []string{namespaceName},
				"storageLocation": storageLocation,
				"ttl": backupTTL,
				"defaultVolumesToFsBackup": defaultVolumesToFsBackup,
			},
		},
	}

	logger.Info(fmt.Sprintf("Creating Velero backup %s", backupName))
	_, err := client.Resource(veleroBackupResource).Namespace("velero").Create(r.Context(), veleroBackup, metav1.CreateOptions{})
	if err != nil {
        logger.Error(fmt.Sprintf("%v", err))
	}
}

func deleteVeleroSchedule(r http.Request, client dynamic.Interface, namespaceName string, logger *logrus.Entry) {
    scheduleName := fmt.Sprintf("%s-backup", namespaceName)

	veleroScheduleResource := schema.GroupVersionResource{
		Group: "velero.io",
		Version: "v1",
		Resource: "schedules",
	}

	logger.Info(fmt.Sprintf("Deleting Velero schedule %s", scheduleName))
	err := client.Resource(veleroScheduleResource).Namespace("velero").Delete(r.Context(), scheduleName, metav1.DeleteOptions{})
	if err != nil {
        logger.Error(fmt.Sprintf("%v", err))
	}
}

func ServerHealth(w http.ResponseWriter, r *http.Request) {
	logrus.WithFields(logrus.Fields{"uri": r.RequestURI}).Debug("healthy")
	w.WriteHeader(http.StatusOK)
    w.Write([]byte("ok"))
}

func parseRequest(r http.Request) (*admissionv1.AdmissionReview, error) {
    if r.Header.Get("Content-Type") != "application/json" {
		return nil, fmt.Errorf("Content-Type: %q should be %q", r.Header.Get("Content-Type"), "application/json")
	}

	bodybuf := new(bytes.Buffer)
	bodybuf.ReadFrom(r.Body)
	body := bodybuf.Bytes()

	if len(body) == 0 {
		return nil, fmt.Errorf("Admission request body is empty")
	}

	var a admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, fmt.Errorf("Failed to parse admission request: %v", err)
	}

	if a.Request == nil {
		return nil, fmt.Errorf("Admission request is empty")
	}

	return &a, nil
}

func setEnv() {
	logger := logrus.WithFields(logrus.Fields{})

	cronExpression = os.Getenv("CRON_EXPRESSION", cronExpression)
	
	csiSnapshotTimeout = os.Getenv("CSI_SNAPSHOT_TIMEOUT", csiSnapshotTimeout)

	storageLocation = os.Getenv("STORAGE_LOCATION", storageLocation)
	
	ttl = os.Getenv("BACKUP_TTL", ttl)
	
	defaultVolumesToFsBackupEnv := os.Getenv("DEFAULT_VOLUMES_TO_FS_BACKUP")
	if defaultVolumesToFsBackupEnv == "" || strings.ToLower(defaultVolumesToFsBackupEnv) == "true" {
		defaultVolumesToFsBackup = true
	} else {
		defaultVolumesToFsBackup = false
	}

	backupSufix = os.Getenv("BACKUP_SUFIX", backupSufix)

	logFormat = os.Getenv("LOG_FORMAT", "text")

	logLevel := os.Getenv("LOG_LEVEL")

	logger.WithFields(logrus.Fields{
        "cronExpression":			cronExpression,
        "csiSnapshotTimeout":		csiSnapshotTimeout,
        "storageLocation":			storageLocation,
        "backupTTL":				ttl,
        "defaultVolumesToFsBackup":	defaultVolumesToFsBackup,
		"backupSufix":				backupSufix,
        "logFormat":				logFormat,
        "logLevel":					logLevel,
    }).Info("Set environment variables")

	if logFormat == "json" {
		logrus.SetFormatter(&logrus.JSONFormatter{})
	} else {
		logrus.SetFormatter(&logrus.TextFormatter{})
	}

	logrus.SetLevel(logrus.DebugLevel)

	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		logrus.Warn("Invalid LOG_LEVEL; using default level")
	} else {
		logrus.SetLevel(level)
	}
}