// Package main provides the docking job controller with REST API
package main

import (
"context"
"fmt"
"log"
"net/http"
"os"
"os/signal"
"syscall"
"time"

batchv1 "k8s.io/api/batch/v1"
corev1 "k8s.io/api/core/v1"
"k8s.io/apimachinery/pkg/api/errors"
metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
"k8s.io/client-go/kubernetes"
typed "k8s.io/client-go/kubernetes/typed/batch/v1"
rest "k8s.io/client-go/rest"
"k8s.io/client-go/tools/clientcmd"
)

const (
// Default values
DefaultImage            = "hwcopeland/auto-docker:latest"
DefaultAutodockPvc      = "pvc-autodock"
DefaultUserPvcPrefix    = "claim-"
DefaultMountPath        = "/data"
DefaultLigandsChunkSize = 10000
DefaultPDBID            = "7jrn"
DefaultLigandDb         = "ChEBI_complete"
DefaultJupyterUser      = "jovyan"
DefaultNativeLigand     = "TTT"

// Finalizer
DockingJobFinalizer = "docking.k8s.io/finalizer"
)

// DockingJobController handles the lifecycle of DockingJob resources
type DockingJobController struct {
client    *kubernetes.Clientset
namespace string
jobClient typed.JobInterface
stopCh    chan struct{}
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
Phase            string     `json:"phase,omitempty"`
BatchCount       int        `json:"batchCount,omitempty"`
CompletedBatches int        `json:"completedBatches,omitempty"`
Message          string     `json:"message,omitempty"`
StartTime        *time.Time `json:"startTime,omitempty"`
CompletionTime   *time.Time `json:"completionTime,omitempty"`
}

// DockingJobList is a list of DockingJob resources
type DockingJobList struct {
metav1.TypeMeta `json:",inline"`
metav1.ListMeta `json:"metadata,omitempty"`
Items           []DockingJob `json:"items"`
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

go func() {
if err := c.startAPIServer(ctx); err != nil && err != http.ErrServerClosed {
log.Printf("API server error: %v", err)
}
}()

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

func (c *DockingJobController) startAPIServer(ctx context.Context) error {
apiHandler := NewAPIHandler(c.client, c.namespace)
mux := http.NewServeMux()

// Logs endpoint must be registered before the dockingjobs prefix
mux.HandleFunc("/api/v1/dockingjobs/logs", func(w http.ResponseWriter, r *http.Request) {
if r.Method == http.MethodGet {
apiHandler.GetLogs(w, r)
} else {
http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}
})

mux.HandleFunc("/api/v1/dockingjobs", func(w http.ResponseWriter, r *http.Request) {
switch r.Method {
case http.MethodGet:
if r.URL.Query().Get("name") != "" {
apiHandler.GetJob(w, r)
} else {
apiHandler.ListJobs(w, r)
}
case http.MethodPost:
c.createJobAndProcess(ctx, w, r)
case http.MethodDelete:
apiHandler.DeleteJob(w, r)
default:
http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}
})

mux.HandleFunc("/health", apiHandler.HealthCheck)

srv := &http.Server{
Addr:         ":8080",
Handler:      mux,
ReadTimeout:  15 * time.Second,
WriteTimeout: 15 * time.Second,
IdleTimeout:  60 * time.Second,
}

go func() {
<-ctx.Done()
shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
if err := srv.Shutdown(shutdownCtx); err != nil {
log.Printf("HTTP server shutdown error: %v", err)
}
}()

log.Println("API server listening on :8080")
return srv.ListenAndServe()
}

// createJobAndProcess handles POST /api/v1/dockingjobs, validates the request,
// returns a 201 response, and starts the workflow in the background.
func (c *DockingJobController) createJobAndProcess(ctx context.Context, w http.ResponseWriter, r *http.Request) {
apiHandler := NewAPIHandler(c.client, c.namespace)

job, req, ok := apiHandler.ParseCreateRequest(w, r)
if !ok {
return
}

// Apply defaults
if req.Image == "" {
req.Image = DefaultImage
}
job.Spec.Image = req.Image
job.Spec.AutodockPvc = DefaultAutodockPvc
job.Spec.UserPvcPrefix = DefaultUserPvcPrefix
job.Spec.MountPath = DefaultMountPath
if req.LigandsChunkSize > 0 {
job.Spec.LigandsChunkSize = req.LigandsChunkSize
} else {
job.Spec.LigandsChunkSize = DefaultLigandsChunkSize
}

apiHandler.WriteCreateResponse(w, job)

go c.processDockingJob(ctx, job)
}

func (c *DockingJobController) processDockingJob(ctx context.Context, job DockingJob) {
log.Printf("Processing docking job: %s", job.Name)

now := time.Now()
job.Status.Phase = "Running"
job.Status.StartTime = &now

// Step 1: Copy ligand DB
copyJobName, err := c.createCopyLigandDbJob(ctx, job)
if err != nil {
c.failJob(job, fmt.Sprintf("copy ligand DB failed: %v", err))
return
}
if err := c.waitForJobCompletion(ctx, copyJobName); err != nil {
c.failJob(job, fmt.Sprintf("copy ligand DB failed: %v", err))
return
}

// Step 2: Prepare receptor
receptorJobName, err := c.createPrepareReceptorJob(ctx, job)
if err != nil {
c.failJob(job, fmt.Sprintf("prepare receptor failed: %v", err))
return
}
if err := c.waitForJobCompletion(ctx, receptorJobName); err != nil {
c.failJob(job, fmt.Sprintf("prepare receptor failed: %v", err))
return
}

// Step 3: Split SDF
splitJobName, err := c.createSplitSdfJob(ctx, job)
if err != nil {
c.failJob(job, fmt.Sprintf("split SDF failed: %v", err))
return
}
if err := c.waitForJobCompletion(ctx, splitJobName); err != nil {
c.failJob(job, fmt.Sprintf("split SDF failed: %v", err))
return
}

// Default batch count - in production, read from job output/annotation
batchCount := 5
job.Status.BatchCount = batchCount

// Step 4: Process batches sequentially
for i := 0; i < batchCount; i++ {
// Use dash to keep names DNS-1123 compliant
batchLabel := fmt.Sprintf("%s-batch%d", job.Spec.LigandDb, i)

prepJobName, err := c.createPrepareLigandsJob(ctx, job, batchLabel)
if err != nil {
c.failJob(job, fmt.Sprintf("prepare ligands batch %d failed: %v", i, err))
return
}
if err := c.waitForJobCompletion(ctx, prepJobName); err != nil {
c.failJob(job, fmt.Sprintf("prepare ligands batch %d failed: %v", i, err))
return
}

dockJobName, err := c.createDockingJobExecution(ctx, job, batchLabel)
if err != nil {
c.failJob(job, fmt.Sprintf("docking batch %d failed: %v", i, err))
return
}
if err := c.waitForJobCompletion(ctx, dockJobName); err != nil {
c.failJob(job, fmt.Sprintf("docking batch %d failed: %v", i, err))
return
}

job.Status.CompletedBatches = i + 1
}

// Step 5: Postprocessing
postJobName, err := c.createPostProcessingJob(ctx, job)
if err != nil {
c.failJob(job, fmt.Sprintf("postprocessing failed: %v", err))
return
}
if err := c.waitForJobCompletion(ctx, postJobName); err != nil {
c.failJob(job, fmt.Sprintf("postprocessing failed: %v", err))
return
}

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

func (c *DockingJobController) createCopyLigandDbJob(ctx context.Context, job DockingJob) (string, error) {
userPvcName := fmt.Sprintf("%s%s", job.Spec.UserPvcPrefix, job.Spec.JupyterUser)
jobName := fmt.Sprintf("%s-copy-ligand", job.Name)

jobSpec := &batchv1.Job{
ObjectMeta: metav1.ObjectMeta{
Name:      jobName,
Namespace: c.namespace,
Labels: map[string]string{
"docking.k8s.io/workflow":   job.Name,
"docking.k8s.io/job-type":   "copy-ligand-db",
"docking.k8s.io/parent-job": job.Name,
},
},
Spec: batchv1.JobSpec{
TTLSecondsAfterFinished: ptrInt32(300),
Template: corev1.PodTemplateSpec{
Spec: corev1.PodSpec{
RestartPolicy: corev1.RestartPolicyOnFailure,
Containers: []corev1.Container{
{
Name:    "copy",
Image:   "alpine:latest",
Command: []string{"/bin/sh", "-c"},
Args: []string{
fmt.Sprintf("cp /user-data/%s.sdf %s/%s.sdf",
job.Spec.LigandDb, job.Spec.MountPath, job.Spec.LigandDb),
},
VolumeMounts: []corev1.VolumeMount{
{Name: "autodock-pvc", MountPath: job.Spec.MountPath},
{Name: "user-pvc", MountPath: "/user-data"},
},
},
},
Volumes: []corev1.Volume{
{
Name: "autodock-pvc",
VolumeSource: corev1.VolumeSource{
PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
ClaimName: job.Spec.AutodockPvc,
},
},
},
{
Name: "user-pvc",
VolumeSource: corev1.VolumeSource{
PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
ClaimName: userPvcName,
},
},
},
},
},
},
},
}

if _, err := c.jobClient.Create(ctx, jobSpec, metav1.CreateOptions{}); err != nil {
return "", err
}
return jobName, nil
}

func (c *DockingJobController) createPrepareReceptorJob(ctx context.Context, job DockingJob) (string, error) {
jobName := fmt.Sprintf("%s-prepare-receptor", job.Name)

jobSpec := &batchv1.Job{
ObjectMeta: metav1.ObjectMeta{
Name:      jobName,
Namespace: c.namespace,
Labels: map[string]string{
"docking.k8s.io/workflow":   job.Name,
"docking.k8s.io/job-type":   "prepare-receptor",
"docking.k8s.io/parent-job": job.Name,
},
},
Spec: batchv1.JobSpec{
TTLSecondsAfterFinished: ptrInt32(300),
Template: corev1.PodTemplateSpec{
Spec: corev1.PodSpec{
RestartPolicy: corev1.RestartPolicyOnFailure,
Containers: []corev1.Container{
{
Name:            "prepare",
Image:           job.Spec.Image,
ImagePullPolicy: corev1.PullAlways,
WorkingDir:      job.Spec.MountPath,
Command:         []string{"python3", "/autodock/scripts/proteinprepv2.py"},
Args: []string{
"--protein_id", job.Spec.PDBID,
"--ligand_id", job.Spec.NativeLigand,
},
VolumeMounts: []corev1.VolumeMount{
{Name: "autodock-pvc", MountPath: job.Spec.MountPath},
},
},
},
Volumes: []corev1.Volume{
{
Name: "autodock-pvc",
VolumeSource: corev1.VolumeSource{
PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
ClaimName: job.Spec.AutodockPvc,
},
},
},
},
},
},
},
}

if _, err := c.jobClient.Create(ctx, jobSpec, metav1.CreateOptions{}); err != nil {
return "", err
}
return jobName, nil
}

func (c *DockingJobController) createSplitSdfJob(ctx context.Context, job DockingJob) (string, error) {
jobName := fmt.Sprintf("%s-split-sdf", job.Name)

jobSpec := &batchv1.Job{
ObjectMeta: metav1.ObjectMeta{
Name:      jobName,
Namespace: c.namespace,
Labels: map[string]string{
"docking.k8s.io/workflow":   job.Name,
"docking.k8s.io/job-type":   "split-sdf",
"docking.k8s.io/parent-job": job.Name,
},
},
Spec: batchv1.JobSpec{
TTLSecondsAfterFinished: ptrInt32(300),
Template: corev1.PodTemplateSpec{
Spec: corev1.PodSpec{
RestartPolicy: corev1.RestartPolicyOnFailure,
Containers: []corev1.Container{
{
Name:            "split",
Image:           job.Spec.Image,
ImagePullPolicy: corev1.PullAlways,
WorkingDir:      job.Spec.MountPath,
Command:         []string{"/bin/sh", "-c"},
Args: []string{
fmt.Sprintf("/autodock/scripts/split_sdf.sh %d %s",
job.Spec.LigandsChunkSize, job.Spec.LigandDb),
},
Env: []corev1.EnvVar{
{Name: "MOUNT_PATH_AUTODOCK", Value: job.Spec.MountPath},
},
VolumeMounts: []corev1.VolumeMount{
{Name: "autodock-pvc", MountPath: job.Spec.MountPath},
},
},
},
Volumes: []corev1.Volume{
{
Name: "autodock-pvc",
VolumeSource: corev1.VolumeSource{
PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
ClaimName: job.Spec.AutodockPvc,
},
},
},
},
},
},
},
}

if _, err := c.jobClient.Create(ctx, jobSpec, metav1.CreateOptions{}); err != nil {
return "", err
}
return jobName, nil
}

func (c *DockingJobController) createPrepareLigandsJob(ctx context.Context, job DockingJob, batchLabel string) (string, error) {
jobName := fmt.Sprintf("%s-prepare-ligands-%s", job.Name, batchLabel)

jobSpec := &batchv1.Job{
ObjectMeta: metav1.ObjectMeta{
Name:      jobName,
Namespace: c.namespace,
Labels: map[string]string{
"docking.k8s.io/workflow":   job.Name,
"docking.k8s.io/job-type":   "prepare-ligands",
"docking.k8s.io/batch":      batchLabel,
"docking.k8s.io/parent-job": job.Name,
},
},
Spec: batchv1.JobSpec{
TTLSecondsAfterFinished: ptrInt32(300),
Template: corev1.PodTemplateSpec{
Spec: corev1.PodSpec{
RestartPolicy: corev1.RestartPolicyOnFailure,
Containers: []corev1.Container{
{
Name:            "prepare",
Image:           job.Spec.Image,
ImagePullPolicy: corev1.PullAlways,
WorkingDir:      job.Spec.MountPath,
Command:         []string{"python3", "/autodock/scripts/ligandprepv2.py"},
Args: []string{
fmt.Sprintf("%s.sdf", batchLabel),
fmt.Sprintf("%s/output", job.Spec.MountPath),
"--format", "pdb",
},
Env: []corev1.EnvVar{
{Name: "MOUNT_PATH_AUTODOCK", Value: job.Spec.MountPath},
},
VolumeMounts: []corev1.VolumeMount{
{Name: "autodock-pvc", MountPath: job.Spec.MountPath},
},
},
},
Volumes: []corev1.Volume{
{
Name: "autodock-pvc",
VolumeSource: corev1.VolumeSource{
PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
ClaimName: job.Spec.AutodockPvc,
},
},
},
},
},
},
},
}

if _, err := c.jobClient.Create(ctx, jobSpec, metav1.CreateOptions{}); err != nil {
return "", err
}
return jobName, nil
}

func (c *DockingJobController) createDockingJobExecution(ctx context.Context, job DockingJob, batchLabel string) (string, error) {
jobName := fmt.Sprintf("%s-docking-%s", job.Name, batchLabel)

jobSpec := &batchv1.Job{
ObjectMeta: metav1.ObjectMeta{
Name:      jobName,
Namespace: c.namespace,
Labels: map[string]string{
"docking.k8s.io/workflow":   job.Name,
"docking.k8s.io/job-type":   "docking",
"docking.k8s.io/batch":      batchLabel,
"docking.k8s.io/parent-job": job.Name,
},
},
Spec: batchv1.JobSpec{
TTLSecondsAfterFinished: ptrInt32(300),
Template: corev1.PodTemplateSpec{
Spec: corev1.PodSpec{
RestartPolicy: corev1.RestartPolicyOnFailure,
Containers: []corev1.Container{
{
Name:            "docking",
Image:           job.Spec.Image,
ImagePullPolicy: corev1.PullAlways,
WorkingDir:      job.Spec.MountPath,
Command:         []string{"/bin/sh", "-c"},
Args: []string{
fmt.Sprintf("/autodock/scripts/dockingv2.sh %s %s",
job.Spec.PDBID, batchLabel),
},
VolumeMounts: []corev1.VolumeMount{
{Name: "autodock-pvc", MountPath: job.Spec.MountPath},
},
},
},
Volumes: []corev1.Volume{
{
Name: "autodock-pvc",
VolumeSource: corev1.VolumeSource{
PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
ClaimName: job.Spec.AutodockPvc,
},
},
},
},
},
},
},
}

if _, err := c.jobClient.Create(ctx, jobSpec, metav1.CreateOptions{}); err != nil {
return "", err
}
return jobName, nil
}

func (c *DockingJobController) createPostProcessingJob(ctx context.Context, job DockingJob) (string, error) {
jobName := fmt.Sprintf("%s-postprocessing", job.Name)

jobSpec := &batchv1.Job{
ObjectMeta: metav1.ObjectMeta{
Name:      jobName,
Namespace: c.namespace,
Labels: map[string]string{
"docking.k8s.io/workflow":   job.Name,
"docking.k8s.io/job-type":   "postprocessing",
"docking.k8s.io/parent-job": job.Name,
},
},
Spec: batchv1.JobSpec{
TTLSecondsAfterFinished: ptrInt32(300),
Template: corev1.PodTemplateSpec{
Spec: corev1.PodSpec{
RestartPolicy: corev1.RestartPolicyOnFailure,
Containers: []corev1.Container{
{
Name:            "postprocess",
Image:           job.Spec.Image,
ImagePullPolicy: corev1.PullAlways,
WorkingDir:      job.Spec.MountPath,
Command:         []string{"/autodock/scripts/3_post_processing.sh"},
Args:            []string{job.Spec.PDBID, job.Spec.LigandDb},
VolumeMounts: []corev1.VolumeMount{
{Name: "autodock-pvc", MountPath: job.Spec.MountPath},
},
},
},
Volumes: []corev1.Volume{
{
Name: "autodock-pvc",
VolumeSource: corev1.VolumeSource{
PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
ClaimName: job.Spec.AutodockPvc,
},
},
},
},
},
},
},
}

if _, err := c.jobClient.Create(ctx, jobSpec, metav1.CreateOptions{}); err != nil {
return "", err
}
return jobName, nil
}

func (c *DockingJobController) waitForJobCompletion(ctx context.Context, jobName string) error {
ticker := time.NewTicker(5 * time.Second)
defer ticker.Stop()
timeout := time.After(10 * time.Minute)

for {
select {
case <-ctx.Done():
return ctx.Err()
case <-timeout:
return fmt.Errorf("timeout waiting for job %s", jobName)
case <-ticker.C:
job, err := c.jobClient.Get(ctx, jobName, metav1.GetOptions{})
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

// ptrInt32 returns a pointer to an int32
func ptrInt32(i int32) *int32 {
return &i
}

func main() {
log.Println("Docking Job Controller starting...")

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
