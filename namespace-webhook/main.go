package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/sirupsen/logrus"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/runtime/schema"
    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

func main() {
	setLogger()

	http.HandleFunc("/validate", ServerCreateNamespaceBackup)
	http.HandleFunc("/health", ServerHealth)

	if os.Getenv("TLS") == "true" {
		cert := "/etc/admission-webhook/tls/tls.crt"
		key := "/etc/admission-webhook/tls/tls.key"
		logrus.Print("Listening on port 443...")
		logrus.Fatal(http.ListenAndServeTLS(":443", cert, key, nil))
	} else {
		logrus.Print("Listening on port 8080...")
		logrus.Fatal(http.ListenAndServe(":8080", nil))
	}
}

func ServerCreateNamespaceBackup(w http.ResponseWriter, r *http.Request) {
	logger := logrus.WithFields(logrus.Fields{"uri": r.RequestURI})
	logger.Debug("Received mutation request")

	admissionReview, err := parseRequest(*r)
	if err != nil {
		logger.WithFields(logrus.Fields{"error": err}).Error("Failed to parse request")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	namespace := corev1.Namespace{}
	if err := json.Unmarshal(admissionReview.Request.Object.Raw, &namespace); err != nil {
		logger.WithFields(logrus.Fields{"error": err}).Error("Failed to parse namespace")
		http.Error(w, fmt.Sprintf("Could not parse namespace: %v", err), http.StatusBadRequest)
		return
	}

    switch admissionReview.Request.Operation {
        case admissionv1.Create:
            logger.Info(fmt.Sprintf("Namespace %s created", namespace.Name))
            // Handle creation logic here
        case admissionv1.Update:
            logger.Info(fmt.Sprintf("Namespace %s updated", namespace.Name))
            // Handle update logic here
        case admissionv1.Delete:
            logger.Info(fmt.Sprintf("Namespace %s deleted", namespace.Name))
            // Handle deletion logic here
        default:
            logger.Info("Unknown operation")
	}

	if target, targetKey := namespace.Labels["namespace.oam.dev/target"]; targetKey {
        if runtime, runtimeKey := namespace.Labels["usage.oam.dev/runtime"]; runtimeKey {
            config, err := rest.InClusterConfig()
            if err != nil {
                logger.WithFields(logrus.Fields{"error": err}).Error("failed to get in-cluster config")
                http.Error(w, fmt.Sprintf("Could not get in-cluster config: %v", err), http.StatusInternalServerError)
                return
            }

            dynamicClient, err := dynamic.NewForConfig(config)
            if err != nil {
				logger.WithFields(logrus.Fields{"error": err}).Error("failed to create clientset")
				http.Error(w, fmt.Sprintf("Could not create clientset: %v", err), http.StatusInternalServerError)
				return
			}

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
						"name": fmt.Sprintf("%s-%s-%s", namespace.Name, runtime, "nginx-example"),
						"namespace": "velero",
					},
					"spec": map[string]interface{}{
						"csiSnapshotTimeout": "10m",
						"defaultVolumesToFsBackup": true,
						"includedNamespaces": []string{"nginx-example"},
						"storageLocation": "default",
						"ttl": "720h0m0s",
					},
                },
            }

			logger.Info(fmt.Sprintf("Creating velero backup %s", veleroBackup.Object["metadata"].(map[string]interface{})["name"]))
			logger.Info(fmt.Sprintf("Target: %s, Runtime: %s", target, runtime))

			_, err = dynamicClient.Resource(veleroBackupResource).Namespace("velero").Create(r.Context(), veleroBackup, metav1.CreateOptions{})
			if err != nil {
				logger.WithFields(logrus.Fields{"error": err}).Error("Failed to create velero backup")
				http.Error(w, fmt.Sprintf("Could not create velero backup: %v", err), http.StatusInternalServerError)
				return
			}
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

func ServerHealth(w http.ResponseWriter, r *http.Request) {
	logrus.WithFields(logrus.Fields{"uri": r.RequestURI}).Debug("healthy")
	w.WriteHeader(http.StatusOK)
    w.Write([]byte("ok"))
}

func setLogger() {
	logrus.SetLevel(logrus.DebugLevel)

	logLevel := os.Getenv("LOG_LEVEL")

	if logLevel != "" {
		logLevel, err := logrus.ParseLevel(logLevel)
		if err != nil {
			logrus.Fatalf("Error setting log level: %v", err)
		}
		logrus.SetLevel(logLevel)
	}

	if os.Getenv("LOG_FORMAT") == "json" {
		logrus.SetFormatter(&logrus.JSONFormatter{})
	}
}

func parseRequest(r http.Request) (*admissionv1.AdmissionReview, error) {
	logrus.Debug("Parsing request")

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