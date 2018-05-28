package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ghodss/yaml"
	"google.golang.org/grpc"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/helm/pkg/chartutil"
	"k8s.io/helm/pkg/helm"
	"k8s.io/helm/pkg/proto/hapi/chart"
	"k8s.io/helm/pkg/proto/hapi/release"
	"k8s.io/helm/pkg/repo"

	helmCrdV1 "github.com/bitnami-labs/helm-crd/pkg/apis/helm.bitnami.com/v1"
	helmClientset "github.com/bitnami-labs/helm-crd/pkg/client/clientset/versioned"
)

const (
	defaultNamespace      = metav1.NamespaceSystem
	defaultRepoURL        = "https://kubernetes-charts.storage.googleapis.com"
	releaseFinalizer      = "helm.bitnami.com/helmrelease"
	defaultTimeoutSeconds = 180
	maxRetries            = 5
)

// Controller is a cache.Controller for acting on Helm CRD objects
type Controller struct {
	queue             workqueue.RateLimitingInterface
	informer          cache.SharedIndexInformer
	kubeClient        kubernetes.Interface
	helmReleaseClient helmClientset.Interface
	helmClient        *helm.Client
	netClient         *http.Client
}

type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// NewController creates a Controller
func NewController(clientset helmClientset.Interface, kubeClient kubernetes.Interface) cache.Controller {
	lw := cache.NewListWatchFromClient(clientset.HelmV1().RESTClient(), "helmreleases", metav1.NamespaceAll, fields.Everything())

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	informer := cache.NewSharedIndexInformer(
		lw,
		&helmCrdV1.HelmRelease{},
		0, // No periodic resync
		cache.Indexers{},
	)

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(newObj)
			if err == nil {
				queue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
	})

	log.Printf("Using tiller host: %s", settings.TillerHost)

	return &Controller{
		informer:          informer,
		queue:             queue,
		kubeClient:        kubeClient,
		helmReleaseClient: clientset,
		helmClient:        helm.NewClient(helm.Host(settings.TillerHost)),
		netClient: &http.Client{
			Timeout: time.Second * defaultTimeoutSeconds,
		},
	}
}

// HasSynced returns true once this controller has completed an
// initial resource listing
func (c *Controller) HasSynced() bool {
	return c.informer.HasSynced()
}

// LastSyncResourceVersion is the resource version observed when last
// synced with the underlying store. The value returned is not
// synchronized with access to the underlying store and is not
// thread-safe.
func (c *Controller) LastSyncResourceVersion() string {
	return c.informer.LastSyncResourceVersion()
}

// Run begins processing items, and will continue until a value is
// sent down stopCh.  It's an error to call Run more than once.  Run
// blocks; call via go.
func (c *Controller) Run(stopCh <-chan struct{}) {
	log.Print("Starting HelmReleases controller")

	defer utilruntime.HandleCrash()

	defer c.queue.ShutDown()

	go c.informer.Run(stopCh)

	// Set up a helm home dir sufficient to fool the rest of helm
	// client code
	os.MkdirAll(settings.Home.Archive(), 0755)
	os.MkdirAll(settings.Home.Repository(), 0755)
	ioutil.WriteFile(settings.Home.RepositoryFile(),
		[]byte("apiVersion: v1\nrepositories: []"), 0644)

	if !cache.WaitForCacheSync(stopCh, c.HasSynced) {
		utilruntime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}
	log.Print("Cache synchronised, starting main loop")

	wait.Until(c.runWorker, time.Second, stopCh)

	log.Print("Shutting down controller")
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
		// continue looping
	}
}

func (c *Controller) processNextItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}

	defer c.queue.Done(key)
	err := c.updateRelease(key.(string))
	if err == nil {
		// No error, reset the ratelimit counters
		c.queue.Forget(key)
	} else if c.queue.NumRequeues(key) < maxRetries {
		log.Printf("Error updating %s, will retry: %v", key, err)
		c.queue.AddRateLimited(key)
	} else {
		// err != nil and too many retries
		log.Printf("Error updating %s, giving up: %v", key, err)
		c.queue.Forget(key)
		utilruntime.HandleError(err)
	}

	return true
}

func fetchUrl(netClient httpClient, reqURL, authHeader string) (*http.Response, error) {
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}

	if len(authHeader) > 0 {
		req.Header.Set("Authorization", authHeader)
	}
	return netClient.Do(req)
}

func fetchRepoIndex(netClient httpClient, repoURL string, authHeader string) (*repo.IndexFile, error) {
	parsedURL, err := url.ParseRequestURI(repoURL)
	if err != nil {
		return nil, err
	}

	res, err := fetchUrl(netClient, parsedURL.String(), authHeader)
	if res != nil {
		defer res.Body.Close()
	}
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		return nil, errors.New("repo index request failed")
	}

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	index := &repo.IndexFile{}
	err = yaml.Unmarshal(body, index)
	if err != nil {
		return index, err
	}
	index.SortEntries()
	return index, nil
}

func findChartInRepoIndex(repoIndex *repo.IndexFile, chartName, chartVersion string) (string, error) {
	errMsg := fmt.Sprintf("chart %q", chartName)
	if chartVersion != "" {
		errMsg = fmt.Sprintf("%s version %q", errMsg, chartVersion)
	}
	cv, err := repoIndex.Get(chartName, chartVersion)
	if err != nil {
		return "", fmt.Errorf("%s not found in repository", errMsg)
	}

	if len(cv.URLs) == 0 {
		return "", fmt.Errorf("%s has no downloadable URLs", errMsg)
	}
	return cv.URLs[0], nil
}

func fetchChart(netClient httpClient, chartURL, authHeader string) (*chart.Chart, error) {
	res, err := fetchUrl(netClient, chartURL, authHeader)
	if res != nil {
		defer res.Body.Close()
	}
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		return nil, errors.New("chart download request failed")
	}

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	return chartutil.LoadArchive(bytes.NewReader(body))
}

func isNotFound(err error) bool {
	// Ideally this would be `grpc.Code(err) == codes.NotFound`,
	// but it seems helm doesn't return grpc codes
	return strings.Contains(grpc.ErrorDesc(err), "not found")
}

func resolveChartURL(index, chart string) (string, error) {
	indexURL, err := url.Parse(strings.TrimSpace(index))
	if err != nil {
		return "", err
	}
	chartURL, err := indexURL.Parse(strings.TrimSpace(chart))
	if err != nil {
		return "", err
	}
	return chartURL.String(), nil
}

func getReleaseName(r *helmCrdV1.HelmRelease) string {
	rname := r.Spec.ReleaseName
	if rname == "" {
		rname = fmt.Sprintf("%s-%s", r.Namespace, r.Name)
	}
	return rname
}

func findIndex(target string, s []string) int {
	for i := range s {
		if s[i] == target {
			return i
		}
	}
	return -1
}

func removeIndex(i int, s []string) []string {
	lastIdx := len(s) - 1
	if i != lastIdx {
		s[i] = s[lastIdx]
	}
	s[lastIdx] = "" // drop reference to string contents
	return s[:lastIdx]
}

// remove item from slice without keeping order
func remove(item string, s []string) ([]string, error) {
	index := findIndex(item, s)
	if index == -1 {
		return []string{}, fmt.Errorf("%s not present in %v", item, s)
	}
	return removeIndex(index, s), nil
}
func hasFinalizer(h *helmCrdV1.HelmRelease) bool {
	currentFinalizers := h.ObjectMeta.Finalizers
	for _, f := range currentFinalizers {
		if f == releaseFinalizer {
			return true
		}
	}
	return false
}

func removeFinalizer(helmObj *helmCrdV1.HelmRelease) *helmCrdV1.HelmRelease {
	helmObjClone := helmObj.DeepCopy()
	newSlice, _ := remove(releaseFinalizer, helmObj.ObjectMeta.Finalizers)
	if len(newSlice) == 0 {
		newSlice = nil
	}
	helmObjClone.ObjectMeta.Finalizers = newSlice
	return helmObjClone
}

func addFinalizer(helmObj *helmCrdV1.HelmRelease) *helmCrdV1.HelmRelease {
	helmObjClone := helmObj.DeepCopy()
	helmObjClone.ObjectMeta.Finalizers = append(helmObjClone.ObjectMeta.Finalizers, releaseFinalizer)
	return helmObjClone
}

func updateHelmRelease(helmReleaseClient helmClientset.Interface, helmObj *helmCrdV1.HelmRelease) error {
	_, err := helmReleaseClient.HelmV1().HelmReleases(helmObj.Namespace).Update(helmObj)
	return err
}

func (c *Controller) updateRelease(key string) error {
	obj, exists, err := c.informer.GetIndexer().GetByKey(key)
	if err != nil {
		return fmt.Errorf("error fetching object with key %s from store: %v", key, err)
	}

	// this is an update when Function API object is actually deleted, we dont need to process anything here
	if !exists {
		log.Printf("HelmRelease object %s not found in the cache, ignoring the deletion update", key)
		return nil
	}

	helmObj := obj.(*helmCrdV1.HelmRelease)

	if helmObj.ObjectMeta.DeletionTimestamp != nil {
		log.Printf("HelmRelease %s marked to be deleted, uninstalling chart", key)
		// If finalizer is removed, then we already processed the delete update, so just return
		if !hasFinalizer(helmObj) {
			return nil
		}
		_, err = c.helmClient.DeleteRelease(getReleaseName(helmObj), helm.DeletePurge(true))
		if err != nil {
			return err
		}

		// remove finalizer from the function object, so that we dont have to process any further and object can be deleted
		helmObjCopy := removeFinalizer(helmObj)
		err = updateHelmRelease(c.helmReleaseClient, helmObjCopy)
		if err != nil {
			log.Printf("Failed to remove finalizer for obj: %s object due to: %v: ", key, err)
			return err
		}
		log.Printf("Release %s has been successfully processed and marked for deletion", key)
		return nil
	}

	if !hasFinalizer(helmObj) {
		helmObjCopy := addFinalizer(helmObj)
		err = updateHelmRelease(c.helmReleaseClient, helmObjCopy)
		if err != nil {
			log.Printf("Error adding finalizer to %s due to: %v: ", key, err)
			return err
		}
	}

	repoURL := helmObj.Spec.RepoURL
	if repoURL == "" {
		// FIXME: Make configurable
		repoURL = defaultRepoURL
	}
	repoURL = strings.TrimSuffix(strings.TrimSpace(repoURL), "/") + "/index.yaml"

	authHeader := ""
	if helmObj.Spec.Auth.Header != nil {
		namespace := os.Getenv("POD_NAMESPACE")
		if namespace == "" {
			namespace = defaultNamespace
		}

		secret, err := c.kubeClient.Core().Secrets(namespace).Get(helmObj.Spec.Auth.Header.SecretKeyRef.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		authHeader = string(secret.Data[helmObj.Spec.Auth.Header.SecretKeyRef.Key])
	}

	log.Printf("Downloading repo %s index...", repoURL)
	repoIndex, err := fetchRepoIndex(c.netClient, repoURL, authHeader)
	if err != nil {
		return err
	}

	chartURL, err := findChartInRepoIndex(repoIndex, helmObj.Spec.ChartName, helmObj.Spec.Version)
	if err != nil {
		return err
	}

	chartURL, err = resolveChartURL(repoURL, chartURL)
	if err != nil {
		return err
	}

	log.Printf("Downloading %s ...", chartURL)
	chartRequested, err := fetchChart(c.netClient, chartURL, authHeader)
	if err != nil {
		return err
	}

	rlsName := getReleaseName(helmObj)
	var rel *release.Release

	_, err = c.helmClient.ReleaseHistory(rlsName, helm.WithMaxHistory(1))
	if err != nil {
		if !isNotFound(err) {
			return err
		}
		log.Printf("Installing release %s into namespace %s", rlsName, helmObj.Namespace)
		res, err := c.helmClient.InstallReleaseFromChart(
			chartRequested,
			helmObj.Namespace,
			helm.ValueOverrides([]byte(helmObj.Spec.Values)),
			helm.ReleaseName(rlsName),
		)
		if err != nil {
			return err
		}
		rel = res.GetRelease()
	} else {
		log.Printf("Updating release %s", rlsName)
		res, err := c.helmClient.UpdateReleaseFromChart(
			rlsName,
			chartRequested,
			helm.UpdateValueOverrides([]byte(helmObj.Spec.Values)),
			//helm.UpgradeForce(true), ?
		)
		if err != nil {
			return err
		}
		rel = res.GetRelease()
	}

	status, err := c.helmClient.ReleaseStatus(rel.Name)
	if err == nil {
		log.Printf("Installed/updated release %s (status %s)", rel.Name, status.Info.Status.Code)
	} else {
		log.Printf("Unable to fetch release status for %s: %v", rel.Name, err)
	}

	return nil
}
