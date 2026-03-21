// Package api provides HTTP handlers for the docking job API
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	dockingv1 "docking.k8s.io/v1"
)

// APIHandler handles HTTP requests for docking jobs
type APIHandler struct {
	client    *kubernetes.Clientset
	namespace string
}

// NewAPIHandler creates a new API handler
func NewAPIHandler(client *kubernetes.Clientset, namespace string) *APIHandler {
	return &APIHandler{
		client:    client,
		namespace: namespace,
	}
}

// DockingJobRequest represents a request to create a new docking job
type DockingJobRequest struct {
	PDBID            string `json:"pdbid"`
	LigandDb         string `json:"ligand_db"`
	JupyterUser      string `json:"jupyter_user"`
	NativeLigand     string `json:"native_ligand"`
	LigandsChunkSize int    `json:"ligands_chunk_size"`
	Image            string `json:"image"`
}

// DockingJobResponse represents a response containing docking job information
type DockingJobResponse struct {
	Name             string     `json:"name"`
	PDBID            string     `json:"pdbid"`
	LigandDb         string     `json:"ligand_db"`
	Status           string     `json:"status"`
	BatchCount       int        `json:"batch_count"`
	CompletedBatches int        `json:"completed_batches"`
	Message          string     `json:"message"`
	CreatedAt        time.Time  `json:"created_at"`
	StartTime        *time.Time `json:"start_time,omitempty"`
	CompletionTime   *time.Time `json:"completion_time,omitempty"`
}

// ListJobs handles GET /api/v1/dockingjobs
func (h *APIHandler) ListJobs(w http.ResponseWriter, r *http.Request) {
	// Get all jobs with the docking label
	jobs, err := h.client.BatchV1().Jobs(h.namespace).List(r.Context(), metav1.ListOptions{
		LabelSelector: "docking.k8s.io/parent-job",
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to list jobs: %v", err), http.StatusInternalServerError)
		return
	}

	// Group jobs by parent workflow
	workflows := make(map[string][]string)
	for _, job := range jobs.Items {
		parentJob := job.Labels["docking.k8s.io/parent-job"]
		if parentJob != "" {
			workflows[parentJob] = append(workflows[parentJob], job.Name)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"workflows": workflows,
		"count":     len(workflows),
	})
}

// CreateJob handles POST /api/v1/dockingjobs
func (h *APIHandler) CreateJob(w http.ResponseWriter, r *http.Request) {
	var req DockingJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
	}

	// Validate required fields
	if req.PDBID == "" {
		req.PDBID = "7jrn"
	}
	if req.LigandDb == "" {
		http.Error(w, "ligand_db is required", http.StatusBadRequest)
		return
	}
	if req.JupyterUser == "" {
		req.JupyterUser = "jovyan"
	}

	// Generate unique job name
	jobName := fmt.Sprintf("docking-%d", time.Now().Unix())

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(DockingJobResponse{
		Name:       jobName,
		PDBID:      req.PDBID,
		LigandDb:   req.LigandDb,
		Status:     "Pending",
		CreatedAt:  time.Now(),
	})
}

// GetJob handles GET /api/v1/dockingjobs/{name}
func (h *APIHandler) GetJob(w http.ResponseWriter, r *http.Request) {
	// Extract job name from request (simplified)
	jobName := r.URL.Query().Get("name")
	if jobName == "" {
		http.Error(w, "name parameter required", http.StatusBadRequest)
		return
	}

	// Get all jobs for this workflow
	jobs, err := h.client.BatchV1().Jobs(h.namespace).List(r.Context(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("docking.k8s.io/parent-job=%s", jobName),
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get job: %v", err), http.StatusInternalServerError)
		return
	}

	// Calculate status based on child jobs
	status := "Pending"
	completed := 0
	total := len(jobs.Items)

	for _, job := range jobs.Items {
		if job.Status.Succeeded > 0 {
			completed++
		} else if job.Status.Failed > 0 {
			status = "Failed"
			break
		}
	}

	if total > 0 && completed == total {
		status = "Completed"
	} else if completed > 0 {
		status = "Running"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(DockingJobResponse{
		Name:             jobName,
		Status:           status,
		CompletedBatches: completed,
		BatchCount:       total,
	})
}

// DeleteJob handles DELETE /api/v1/dockingjobs/{name}
func (h *APIHandler) DeleteJob(w http.ResponseWriter, r *http.Request) {
	jobName := r.URL.Query().Get("name")
	if jobName == "" {
		http.Error(w, "name parameter required", http.StatusBadRequest)
		return
	}

	// Delete all jobs with this parent label
	jobs, err := h.client.BatchV1().Jobs(h.namespace).List(r.Context(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("docking.k8s.io/parent-job=%s", jobName),
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to list jobs: %v", err), http.StatusInternalServerError)
		return
	}

	for _, job := range jobs.Items {
		if err := h.client.BatchV1().Jobs(h.namespace).Delete(r.Context(), job.Name, metav1.DeleteOptions{}); err != nil {
			log.Printf("Failed to delete job %s: %v", job.Name, err)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// GetLogs handles GET /api/v1/dockingjobs/{name}/logs
func (h *APIHandler) GetLogs(w http.ResponseWriter, r *http.Request) {
	jobName := r.URL.Query().Get("name")
	taskType := r.URL.Query().Get("task")

	if jobName == "" {
		http.Error(w, "name parameter required", http.StatusBadRequest)
		return
	}

	// Find the specific job
	labelSelector := fmt.Sprintf("docking.k8s.io/parent-job=%s", jobName)
	if taskType != "" {
		labelSelector = fmt.Sprintf("docking.k8s.io/parent-job=%s,docking.k8s.io/job-type=%s", jobName, taskType)
	}

	jobs, err := h.client.BatchV1().Jobs(h.namespace).List(r.Context(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil || len(jobs.Items) == 0 {
		http.Error(w, "Job not found", http.StatusNotFound)
		return
	}

	// Get pods for the job
	pods, err := h.client.CoreV1().Pods(h.namespace).List(r.Context(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("job-name=%s", jobs.Items[0].Name),
	})
	if err != nil || len(pods.Items) == 0 {
		http.Error(w, "Pods not found", http.StatusNotFound)
		return
	}

	// Get logs from the first pod
	logs, err := h.client.CoreV1().Pods(h.namespace).GetLogs(pods.Items[0].Name, &metav1.PodLogOptions{}).
		Do(r.Context()).Raw()
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get logs: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write(logs)
}

// HealthCheck handles GET /health
func (h *APIHandler) HealthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "healthy",
		"time":   time.Now().Format(time.RFC3339),
	})
}
