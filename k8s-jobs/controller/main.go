// Package main provides the docking job controller with REST API
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typed "k8s.io/client-go/kubernetes/typed/batch/v1"
	rest "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	dockingv1 "docking.k8s.io/v1"
)

const (
	// Default values
	DefaultImage             = "hwcopeland/auto-docker:latest"
	DefaultAutodockPvc       = "pvc-autodock"
	DefaultUserPvcPrefix     = "claim-"
	DefaultMountPath         = "/data"
	DefaultLigandsChunkSize  = 10000
	DefaultPDBID             = "7jrn"
	DefaultLigandDb          = "ChEBI_complete"
	DefaultJupyterUser       = "jovyan"
	DefaultNativeLigand      = "TTT"

	// Finalizer
	DockingJobFinalizer = "docking.k8s.io/finalizer"
)

// DockingJobController handles the lifecycle of DockingJob resources
type DockingJobController struct {
	client        *kubernetes.Clientset
	namespace     string
	jobClient     typed.JobInterface
	dockingClient *DockingJobClient
	stopCh        chan struct{}
}

// DockingJobClient handles CRUD operations for DockingJob resources
type DockingJobClient struct {
	restClient *rest.RESTClient
	scheme     *runtime.Scheme
}

// DockingJob represents the custom resource
type DockingJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DockingJobSpec   `json:"spec,omitempty"`
	Status            DockingJobStatus `json:"status,omitempty"`
}

type DockingJobSpec struct {
	PDBID            string `json:"pdbid,omitempty"`
	LigandDb         string `json:"ligandDb,omitempty"`
	JupyterUser      string `json:"jupyterUser,omitempty"`
	NativeLigand     string `json:"nativeLigand,omitempty"`
	LigandsChunkSize int    `json:"ligandsChunkSize,omitempty"`
	Image            string `json:"image,omitempty"`
	AutodockPvc      string `json:"autodockPvc,omitempty"`
	UserPvcPrefix    string `json:"userPvcPrefix,omitempty"`
	MountPath        string `json:"mountPath,omitempty"`
}

type DockingJobStatus struct {
	Phase             string     `json:"phase,omitempty"`
	BatchCount        int        `json:"batchCount,omitempty"`
	CompletedBatches  int        `json:"completedBatches,omitempty"`
	Message           string     `json:"message,omitempty"`
	StartTime         *time.Time `json:"startTime,omitempty"`
	CompletionTime    *time.Time `json:"completionTime,omitempty"`
}

// NewDockingJobController creates a new controller
func NewDockingJobController() (*DockingJobController, error) {
	config, err := getConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %v", err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %v", err)
	}

	namespace := os.Getenv("NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}

	return &DockingJobController{
		client:    client,
		namespace: namespace,
		jobClient: client.BatchV1().Jobs(namespace),
		stopCh:    make(chan struct{}),
	}, nil
}

func getConfig() (*rest.Config, error) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

// Run starts the controller
func (c *DockingJobController) Run(ctx context.Context) error {
	log.Println("Starting Docking Job Controller...")

	// Start the API server
	go func() {
		if err := c.startAPIServer(); err != nil {
			log.Printf("API server error: %v", err)
		}
	}()

	// Watch for DockingJob changes (simplified - in production use informers)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := c.reconcileJobs(); err != nil {
				log.Printf("Reconciliation error: %v", err)
			}
		}
	}
}

func (c *DockingJobController) startAPIServer() error {
	http.HandleFunc("/api/v1/dockingjobs", c.handleDockingJobs)
	http.HandleFunc("/api/v1/dockingjobs/", c.handleDockingJob)
	
	// Health check endpoint
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
	})

	log.Println("API server listening on :8080")
	return http.ListenAndServe(":8080", nil)
}

func (c *DockingJobController) handleDockingJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		c.listDockingJobs(w, r)
	case http.MethodPost:
		c.createDockingJob(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (c *DockingJobController) handleDockingJob(w http.ResponseWriter, r *http.Request) {
	// Extract job name from URL path
	// In production, use proper routing
	
	switch r.Method {
	case http.MethodGet:
		c.getDockingJob(w, r)
	case http.MethodDelete:
		c.deleteDockingJob(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (c *DockingJobController) listDockingJobs(w http.ResponseWriter, r *http.Request) {
	// For now, return empty list - in production, query from K8s
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(DockingJobList{})
}

func (c *DockingJobController) createDockingJob(w http.ResponseWriter, r *http.Request) {
	var job DockingJob
	if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Set defaults
	if job.Spec.Image == "" {
		job.Spec.Image = DefaultImage
	}
	if job.Spec.AutodockPvc == "" {
		job.Spec.AutodockPvc = DefaultAutodockPvc
	}
	if job.Spec.UserPvcPrefix == "" {
		job.Spec.UserPvcPrefix = DefaultUserPvcPrefix
	}
	if job.Spec.MountPath == "" {
		job.Spec.MountPath = DefaultMountPath
	}
	if job.Spec.PDBID == "" {
		job.Spec.PDBID = DefaultPDBID
	}
	if job.Spec.LigandDb == "" {
		job.Spec.LigandDb = DefaultLigandDb
	}
	if job.Spec.JupyterUser == "" {
		job.Spec.JupyterUser = DefaultJupyterUser
	}
	if job.Spec.NativeLigand == "" {
		job.Spec.NativeLigand = DefaultNativeLigand
	}
	if job.Spec.LigandsChunkSize == 0 {
		job.Spec.LigandsChunkSize = DefaultLigandsChunkSize
	}

	// Generate name if not provided
	if job.Name == "" {
		job.Name = fmt.Sprintf("docking-%d", time.Now().Unix())
	}

	job.Status.Phase = "Pending"

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(job)
	
	// Start job processing in background
	go c.processDockingJob(job)
}

func (c *DockingJobController) getDockingJob(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(DockingJob{})
}

func (c *DockingJobController) deleteDockingJob(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (c *DockingJobController) processDockingJob(job DockingJob) {
	log.Printf("Processing docking job: %s", job.Name)

	// Update status to Running
	now := time.Now()
	job.Status.Phase = "Running"
	job.Status.StartTime = &now

	// Step 1: Copy ligand DB
	if err := c.createCopyLigandDbJob(job); err != nil {
		c.failJob(job, fmt.Sprintf("copy ligand DB failed: %v", err))
		return
	}

	// Step 2: Prepare receptor
	if err := c.createPrepareReceptorJob(job); err != nil {
		c.failJob(job, fmt.Sprintf("prepare receptor failed: %v", err))
		return
	}

	// Step 3: Split SDF
	batchCount, err := c.createSplitSdfJob(job)
	if err != nil {
		c.failJob(job, fmt.Sprintf("split SDF failed: %v", err))
		return
	}

	job.Status.BatchCount = batchCount

	// Step 4: Process batches (parallel docking jobs)
	for i := 0; i < batchCount; i++ {
		batchLabel := fmt.Sprintf("%s_batch%d", job.Spec.LigandDb, i)
		
		// Create prepare ligands job
		if err := c.createPrepareLigandsJob(job, batchLabel); err != nil {
			c.failJob(job, fmt.Sprintf("prepare ligands batch %d failed: %v", i, err))
			return
		}

		// Create docking job
		if err := c.createDockingJobExecution(job, batchLabel); err != nil {
			c.failJob(job, fmt.Sprintf("docking batch %d failed: %v", i, err))
			return
		}

		job.Status.CompletedBatches = i + 1
	}

	// Step 5: Postprocessing
	if err := c.createPostProcessingJob(job); err != nil {
		c.failJob(job, fmt.Sprintf("postprocessing failed: %v", err))
		return
	}

	// Mark as completed
	completionTime := time.Now()
	job.Status.Phase = "Completed"
	job.Status.CompletionTime = &completionTime
	job.Status.Message = "All steps completed successfully"
	
	log.Printf("Docking job %s completed successfully", job.Name)
}

func (c *DockingJobController) failJob(job DockingJob, message string) {
	job.Status.Phase = "Failed"
	job.Status.Message = message
	log.Printf("Docking job %s failed: %s", job.Name, message)
}

func (c *DockingJobController) createCopyLigandDbJob(job DockingJob) error {
	userPvcPath := fmt.Sprintf("%s%s", job.Spec.UserPvcPrefix, job.Spec.JupyterUser)
	
	jobSpec := &Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-copy-ligand", job.Name),
			Labels: map[string]string{
				"docking.k8s.io/workflow":  job.Name,
				"docking.k8s.io/job-type":   "copy-ligand-db",
				"docking.k8s.io/parent-job": job.Name,
			},
		},
		Spec: JobSpec{
			TTLSecondsAfterFinished: ptr.Int32(300),
			Template: PodTemplateSpec{
				Spec: PodSpec{
					RestartPolicy: "OnFailure",
					Containers: []Container{
						{
							Name:  "copy",
							Image: "alpine:latest",
							Command: []string{"/bin/sh", "-c"},
							Args: []string{
								fmt.Sprintf("cp %s/%s/%s.sdf %s/%s.sdf",
									job.Spec.MountPath, userPvcPath, job.Spec.LigandDb,
									job.Spec.MountPath, job.Spec.LigandDb),
							},
							VolumeMounts: []VolumeMount{
								{
									Name:      "autodock-pvc",
									MountPath: job.Spec.MountPath,
								},
							},
						},
					},
					Volumes: []Volume{
						{
							Name: "autodock-pvc",
							VolumeSource: VolumeSource{
								PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{
									ClaimName: job.Spec.AutodockPvc,
								},
							},
						},
					},
				},
			},
		},
	}

	_, err := c.jobClient.Create(context.TODO(), jobSpec, metav1.CreateOptions{})
	return err
}

func (c *DockingJobController) createPrepareReceptorJob(job DockingJob) error {
	jobSpec := &Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-prepare-receptor", job.Name),
			Labels: map[string]string{
				"docking.k8s.io/workflow":  job.Name,
				"docking.k8s.io/job-type":   "prepare-receptor",
				"docking.k8s.io/parent-job": job.Name,
			},
		},
		Spec: JobSpec{
			TTLSecondsAfterFinished: ptr.Int32(300),
			Template: PodTemplateSpec{
				Spec: PodSpec{
					RestartPolicy: "OnFailure",
					Containers: []Container{
						{
							Name:            "prepare",
							Image:           job.Spec.Image,
							ImagePullPolicy: "Always",
							WorkingDir:      job.Spec.MountPath,
							Command:         []string{"python3", "/autodock/scripts/proteinprepv2.py"},
							Args: []string{
								"--protein_id", job.Spec.PDBID,
								"--ligand_id", job.Spec.NativeLigand,
							},
							VolumeMounts: []VolumeMount{
								{
									Name:      "autodock-pvc",
									MountPath: job.Spec.MountPath,
								},
							},
						},
					},
					Volumes: []Volume{
						{
							Name: "autodock-pvc",
							VolumeSource: VolumeSource{
								PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{
									ClaimName: job.Spec.AutodockPvc,
								},
							},
						},
					},
				},
			},
		},
	}

	_, err := c.jobClient.Create(context.TODO(), jobSpec, metav1.CreateOptions{})
	return err
}

func (c *DockingJobController) createSplitSdfJob(job DockingJob) (int, error) {
	jobSpec := &Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-split-sdf", job.Name),
			Labels: map[string]string{
				"docking.k8s.io/workflow":  job.Name,
				"docking.k8s.io/job-type":   "split-sdf",
				"docking.k8s.io/parent-job": job.Name,
			},
		},
		Spec: JobSpec{
			TTLSecondsAfterFinished: ptr.Int32(300),
			Template: PodTemplateSpec{
				Spec: PodSpec{
					RestartPolicy: "OnFailure",
					Containers: []Container{
						{
							Name:            "split",
							Image:           job.Spec.Image,
							ImagePullPolicy: "Always",
							WorkingDir:      job.Spec.MountPath,
							Command:         []string{"/bin/sh", "-c"},
							Args: []string{
								fmt.Sprintf("/autodock/scripts/split_sdf.sh %d %s",
									job.Spec.LigandsChunkSize, job.Spec.LigandDb),
							},
							Env: []EnvVar{
								{
									Name:  "MOUNT_PATH_AUTODOCK",
									Value: job.Spec.MountPath,
								},
							},
							VolumeMounts: []VolumeMount{
								{
									Name:      "autodock-pvc",
									MountPath: job.Spec.MountPath,
								},
							},
						},
					},
					Volumes: []Volume{
						{
							Name: "autodock-pvc",
							VolumeSource: VolumeSource{
								PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{
									ClaimName: job.Spec.AutodockPvc,
								},
							},
						},
					},
				},
			},
		},
	}

	createdJob, err := c.jobClient.Create(context.TODO(), jobSpec, metav1.CreateOptions{})
	if err != nil {
		return 0, err
	}

	// Wait for job completion
	if err := c.waitForJobCompletion(createdJob.Name); err != nil {
		return 0, err
	}

	// In a real implementation, we'd parse the output to get the batch count
	// For now, return a default value
	return 5, nil
}

func (c *DockingJobController) createPrepareLigandsJob(job DockingJob, batchLabel string) error {
	jobSpec := &Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-prepare-ligands-%s", job.Name, batchLabel),
			Labels: map[string]string{
				"docking.k8s.io/workflow":  job.Name,
				"docking.k8s.io/job-type":   "prepare-ligands",
				"docking.k8s.io/batch":       batchLabel,
				"docking.k8s.io/parent-job": job.Name,
			},
		},
		Spec: JobSpec{
			TTLSecondsAfterFinished: ptr.Int32(300),
			Template: PodTemplateSpec{
				Spec: PodSpec{
					RestartPolicy: "OnFailure",
					Containers: []Container{
						{
							Name:            "prepare",
							Image:           job.Spec.Image,
							ImagePullPolicy: "Always",
							WorkingDir:      job.Spec.MountPath,
							Command:         []string{"python3", "/autodock/scripts/ligandprepv2.py"},
							Args: []string{
								fmt.Sprintf("%s.sdf", batchLabel),
								fmt.Sprintf("%s/output", job.Spec.MountPath),
								"--format", "pdb",
							},
							Env: []EnvVar{
								{
									Name:  "MOUNT_PATH_AUTODOCK",
									Value: job.Spec.MountPath,
								},
							},
							VolumeMounts: []VolumeMount{
								{
									Name:      "autodock-pvc",
									MountPath: job.Spec.MountPath,
								},
							},
						},
					},
					Volumes: []Volume{
						{
							Name: "autodock-pvc",
							VolumeSource: VolumeSource{
								PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{
									ClaimName: job.Spec.AutodockPvc,
								},
							},
						},
					},
				},
			},
		},
	}

	_, err := c.jobClient.Create(context.TODO(), jobSpec, metav1.CreateOptions{})
	return err
}

func (c *DockingJobController) createDockingJobExecution(job DockingJob, batchLabel string) error {
	jobSpec := &Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-docking-%s", job.Name, batchLabel),
			Labels: map[string]string{
				"docking.k8s.io/workflow":  job.Name,
				"docking.k8s.io/job-type":   "docking",
				"docking.k8s.io/batch":       batchLabel,
				"docking.k8s.io/parent-job": job.Name,
			},
		},
		Spec: JobSpec{
			TTLSecondsAfterFinished: ptr.Int32(300),
			Template: PodTemplateSpec{
				Spec: PodSpec{
					RestartPolicy: "OnFailure",
					Containers: []Container{
						{
							Name:            "docking",
							Image:           job.Spec.Image,
							ImagePullPolicy: "Always",
							WorkingDir:      job.Spec.MountPath,
							Command:         []string{"/bin/sh", "-c"},
							Args: []string{
								fmt.Sprintf("/autodock/scripts/dockingv2.sh %s %s",
									job.Spec.PDBID, batchLabel),
							},
							VolumeMounts: []VolumeMount{
								{
									Name:      "autodock-pvc",
									MountPath: job.Spec.MountPath,
								},
							},
						},
					},
					Volumes: []Volume{
						{
							Name: "autodock-pvc",
							VolumeSource: VolumeSource{
								PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{
									ClaimName: job.Spec.AutodockPvc,
								},
							},
						},
					},
				},
			},
		},
	}

	_, err := c.jobClient.Create(context.TODO(), jobSpec, metav1.CreateOptions{})
	return err
}

func (c *DockingJobController) createPostProcessingJob(job DockingJob) error {
	jobSpec := &Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-postprocessing", job.Name),
			Labels: map[string]string{
				"docking.k8s.io/workflow":  job.Name,
				"docking.k8s.io/job-type":   "postprocessing",
				"docking.k8s.io/parent-job": job.Name,
			},
		},
		Spec: JobSpec{
			TTLSecondsAfterFinished: ptr.Int32(300),
			Template: PodTemplateSpec{
				Spec: PodSpec{
					RestartPolicy: "OnFailure",
					Containers: []Container{
						{
							Name:            "postprocess",
							Image:           job.Spec.Image,
							ImagePullPolicy: "Always",
							WorkingDir:      job.Spec.MountPath,
							Command:         []string{"/autodock/scripts/3_post_processing.sh"},
							Args: []string{
								job.Spec.PDBID,
								job.Spec.LigandDb,
							},
							VolumeMounts: []VolumeMount{
								{
									Name:      "autodock-pvc",
									MountPath: job.Spec.MountPath,
								},
							},
						},
					},
					Volumes: []Volume{
						{
							Name: "autodock-pvc",
							VolumeSource: VolumeSource{
								PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{
									ClaimName: job.Spec.AutodockPvc,
								},
							},
						},
					},
				},
			},
		},
	}

	_, err := c.jobClient.Create(context.TODO(), jobSpec, metav1.CreateOptions{})
	return err
}

func (c *DockingJobController) waitForJobCompletion(jobName string) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	timeout := time.After(10 * time.Minute)

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for job %s", jobName)
		case <-ticker.C:
			job, err := c.jobClient.Get(context.TODO(), jobName, metav1.GetOptions{})
			if err != nil {
				if errors.IsNotFound(err) {
					continue
				}
				return err
			}
			if job.Status.Succeeded > 0 {
				return nil
			}
			if job.Status.Failed > 0 {
				return fmt.Errorf("job %s failed", jobName)
			}
		}
	}
}

func (c *DockingJobController) reconcileJobs() error {
	// In a production implementation, this would use an informer to watch
	// DockingJob resources and reconcile them
	return nil
}

// Minimal type definitions to avoid full k8s.io/api/batch/v1 imports
type Job struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              JobSpec `json:"spec,omitempty"`
}

type JobSpec struct {
	TTLSecondsAfterFinished *int32           `json:"ttlSecondsAfterFinished,omitempty"`
	Template                PodTemplateSpec  `json:"template,omitempty"`
}

type PodTemplateSpec struct {
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PodSpec `json:"spec,omitempty"`
}

type PodSpec struct {
	RestartPolicy      string            `json:"restartPolicy,omitempty"`
	Containers         []Container       `json:"containers,omitempty"`
	Volumes            []Volume          `json:"volumes,omitempty"`
}

type Container struct {
	Name            string        `json:"name,omitempty"`
	Image           string        `json:"image,omitempty"`
	ImagePullPolicy string        `json:"imagePullPolicy,omitempty"`
	Command         []string      `json:"command,omitempty"`
	Args            []string      `json:"args,omitempty"`
	WorkingDir      string        `json:"workingDir,omitempty"`
	Env             []EnvVar      `json:"env,omitempty"`
	VolumeMounts    []VolumeMount `json:"volumeMounts,omitempty"`
	Resources       ResourceRequirements `json:"resources,omitempty"`
}

type EnvVar struct {
	Name  string `json:"name,omitempty"`
	Value string `json:"value,omitempty"`
}

type VolumeMount struct {
	Name      string `json:"name,omitempty"`
	MountPath string `json:"mountPath,omitempty"`
}

type Volume struct {
	Name        string       `json:"name,omitempty"`
	VolumeSource VolumeSource `json:"volumeSource,omitempty"`
}

type VolumeSource struct {
	PersistentVolumeClaim *PersistentVolumeClaimVolumeSource `json:"persistentVolumeClaim,omitempty"`
}

type PersistentVolumeClaimVolumeSource struct {
	ClaimName string `json:"claimName,omitempty"`
}

type ResourceRequirements struct {
	Limits   ResourceList `json:"limits,omitempty"`
	Requests ResourceList `json:"requests,omitempty"`
}

type ResourceList map[string]resource.Quantity

type DockingJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DockingJob `json:"items"`
}

// ptrInt32 returns a pointer to an int32
func ptrInt32(i int32) *int32 {
	return &i
}

func main() {
	log.Println("Docking Job Controller starting...")

	// Set up signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("Shutting down...")
		cancel()
	}()

	controller, err := NewDockingJobController()
	if err != nil {
		log.Fatalf("Failed to create controller: %v", err)
	}

	if err := controller.Run(ctx); err != nil {
		log.Fatalf("Controller error: %v", err)
	}
}
