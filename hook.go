package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"

	"github.com/elafarge/k8s-dockerhub-sync/utils"
	kubeAppsV1 "k8s.io/api/apps/v1"
	kubeV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kubeRest "k8s.io/client-go/rest"
)

const (
	// DockerhubWebhookSizeLimit is the maximum size we accept from DockerHub's payload
	// (anti DDoS protection)
	DockerhubWebhookSizeLimit = 128 * 1024
)

// MonitoredDeployments is a lockable list of deployments to monitor and update when the
// corresponding image and tag is pushed to DockerHub. It is locked on automatic refreshes of the
// list.
type MonitoredDeployments struct {
	sync.Mutex

	ImageToDeploymentsMap map[string][]kubeAppsV1.Deployment
}

// DockerhubWebhookPayload describes the payload sent by Dockerhub's webhook
type DockerhubWebhookPayload struct {
	PushData struct {
		Tag string `json:"tag"`
	} `json:"push_data"`

	Repository struct {
		RepoName string `json:"repo_name"`
	} `json:"repository"`
}

// UpdateImageToDeploymentMap updates the image-deployment map
func UpdateImageToDeploymentMap(kubeClientSet *kubernetes.Clientset, namespaces []string, mds *MonitoredDeployments) error {
	mds.Lock()
	defer mds.Unlock()

	// Reset the map !
	mds.ImageToDeploymentsMap = map[string][]kubeAppsV1.Deployment{}

	// And update our list of images to watch for changes
	for _, namespace := range namespaces {
		deploymentsClient := kubeClientSet.AppsV1().Deployments(namespace)
		deployments, err := deploymentsClient.List(kubeV1.ListOptions{})
		if err != nil {
			return fmt.Errorf("Error listing deployments in namespace %s: %v", namespace, err)
		}

		for _, deployment := range deployments.Items {
			encounteredImages := map[string]struct{}{}
			for _, container := range deployment.Spec.Template.Spec.Containers {

				if _, alreadyIn := encounteredImages[container.Image]; !alreadyIn {
					mds.ImageToDeploymentsMap[container.Image] = append(mds.ImageToDeploymentsMap[container.Image], deployment)
					encounteredImages[container.Image] = struct{}{}
				}
			}
		}
	}
	return nil
}

func main() {

	var (
		kubeconfig    string
		syncPeriod    time.Duration
		namespaceList string

		logLevel string
	)

	flag.StringVar(&kubeconfig, "kubeconfig", "cluster", "Path to your kube config file, leave unset for in-cluster automatic configuration")
	flag.StringVar(&namespaceList, "watch-namespaces", "default", "The list of namespaces to watch for image updates")
	flag.DurationVar(&syncPeriod, "sync-period", time.Minute, "Period between two refreshes of the list of Deployments to monitor")
	flag.StringVar(&logLevel, "log-level", "info", "Logrus log level (debug, info, warn, error)")

	flag.Parse()

	log := logrus.WithField("ctx", "main")

	logrusLogLevel, err := logrus.ParseLevel(logLevel)
	if err != nil {
		log.Panicf("Error parsing logrus log level: %v", err)
	}

	// Set log level
	logrus.SetLevel(logrusLogLevel)

	// Format logs as JSON
	logrus.SetFormatter(&utils.FluentdFormatter{"2006-01-02T15:04:05.000000000Z"})

	var watchedNamespaces = strings.Split(namespaceList, ",")
	if len(watchedNamespaces) <= 0 {
		log.Panicln("You should specify at least on namespace to watch in -watch-namespaces")
	}

	// Let's create our Kubernetes client
	var kubeConfig *kubeRest.Config

	if kubeconfig == "cluster" {
		if kubeConfig, err = kubeRest.InClusterConfig(); err != nil {
			log.Panicf("Impossible to retrieve in-cluster Kubernetes credentials: %v", err)
		}
	} else {
		log.Panicln("Fetching config from KubeConfig file hasn't been implemented yet !")
	}

	kubeClientSet, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		panic(fmt.Errorf("Impossible to create Kubernetes client set: %v", err))
	}

	// Let's create (and maintain up to date) the list of deployments matching our image
	monitoredDeployments := MonitoredDeployments{}
	updateGoroutineStopChan := make(chan struct{}, 1)
	var wg sync.WaitGroup

	go func() {
		log := log.WithField("ctx", "deployment-sync")

		wg.Add(1)
		timeTicker := time.Tick(syncPeriod)

	syncloop:
		for {
			log.Debugln("Starting syncloop iteration")

			select {
			case <-updateGoroutineStopChan:
				log.Infoln("Stop message received, exiting")
				break syncloop
			case <-timeTicker:
				if err := UpdateImageToDeploymentMap(kubeClientSet, watchedNamespaces, &monitoredDeployments); err != nil {
					log.Errorf("Error updating images-to-deployments map: %v", err)
				}

				jsonLog := map[string][]string{}
				for image, depls := range monitoredDeployments.ImageToDeploymentsMap {
					jsonLog[image] = []string{}
					for _, depl := range depls {
						jsonLog[image] = append(jsonLog[image], fmt.Sprintf("%s/%s", depl.Namespace, depl.Name))
					}
				}
				jsonLogBytes, err := json.Marshal(jsonLog)
				if err != nil {
					log.Errorf("Error marshaling image -> deployments map to json: %v", err)
				} else {
					log.Debugf("Updated image-to-deployments map: %s", jsonLogBytes)
				}
			}
		}

		wg.Done()
	}()

	// Let's register our routes
	router := mux.NewRouter()

	router.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("Hook is healthy, webserver is running"))
	})

	router.HandleFunc("/dockerhub-push", func(w http.ResponseWriter, r *http.Request) {
		log := logrus.WithField("ctx", "dockerhub-push")

		defer func() {
			if err := r.Body.Close(); err != nil {
				log.Errorf("Error closing dockerhub's request body: %v", err)
			}
		}()

		// Unmarshal Docker's JSON body
		body, err := ioutil.ReadAll(io.LimitReader(r.Body, DockerhubWebhookSizeLimit))
		if err != nil {
			log.Errorf("Error reading Dockerhub's request body: %v", err)
			w.WriteHeader(500)
			return
		}

		var dockerHubPayload DockerhubWebhookPayload
		if err := json.Unmarshal(body, &dockerHubPayload); err != nil {
			log.Errorf("Error unmarshaling Dockerhub's request body to JSON: %v", err)
			w.WriteHeader(400)
			return
		}

		tag := dockerHubPayload.PushData.Tag
		repoName := dockerHubPayload.Repository.RepoName
		imageName := fmt.Sprintf("%s:%s", repoName, tag)

		// Do we have Deployments using the deployed image ? If so, let's do that !
		var updatedDeployments, failedUpdates []string
		monitoredDeployments.Lock()
		if matchList, ok := monitoredDeployments.ImageToDeploymentsMap[imageName]; ok {
			for _, deployment := range matchList {
				logDep := log.WithField("namespace", deployment.Namespace).WithField("deployment", deployment.Name)
				logDep.Infof(
					"Recently pushed image:tag %s matched with deployment %s in namespace %s, updating deployment...",
					imageName, deployment.Name, deployment.Namespace,
				)

				logDep.Debugf("Getting latest revision of the %s/%s deployment before applying update")

				deploymentsClient := kubeClientSet.AppsV1().Deployments(deployment.Namespace)
				deploymentPtr, err := deploymentsClient.Get(deployment.Name, kubeV1.GetOptions{})
				if err != nil {
					logDep.Errorf(
						"Error refreshing deployment object %s/%s: %v",
						deployment.Namespace, deployment.Name, err,
					)
					continue
				}

				// TODO: find a less hacky way to trigger the rolling update and - more important - follow
				// the status of rolling updates and log errors
				if deploymentPtr.Spec.Template.Annotations == nil {
					deploymentPtr.Spec.Template.Annotations = map[string]string{}
				}
				deploymentPtr.Spec.Template.Annotations["dockerhub-sync.io/deployment-seed"] = time.Now().Format(time.UnixDate)

				if _, err := deploymentsClient.Update(deploymentPtr); err != nil {
					logDep.Errorf("Error updating deployment: %v", err)
					failedUpdates = append(failedUpdates, fmt.Sprintf("%s/%s", deploymentPtr.Namespace, deploymentPtr.Name))
				} else {
					logDep.Infof("Updated deployment")
					updatedDeployments = append(updatedDeployments, fmt.Sprintf("%s/%s", deploymentPtr.Namespace, deploymentPtr.Name))
				}
			}
		}
		monitoredDeployments.Unlock()

		if len(failedUpdates) > 0 {
			w.WriteHeader(500)
			w.Write([]byte(fmt.Sprintf("Failed deployments: %v", failedUpdates)))
		} else {
			w.WriteHeader(200)
		}
		w.Write([]byte(fmt.Sprintf("Updated deployments: %v", updatedDeployments)))
	})

	// Let's create the WebServer
	server := http.Server{
		Addr:    ":8080",
		Handler: router,
	}

	// Handle graceful sigterms
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT)

		// Blocks until we actually receive one of these signals
		sig := <-sigChan
		log.Infof("Received %v signal, exiting gracefully...", sig)

		log.Infoln("Stopping sync goroutine...")
		updateGoroutineStopChan <- struct{}{}
		wg.Wait()

		log.Infoln("Shutting down webserver")
		if err := server.Shutdown(context.Background()); err != nil {
			log.Errorf("Error shutting down hook server: %v", err)
		}
	}()

	log.Debugln("Starting webhook server")
	if err := server.ListenAndServe(); err != nil {
		log.Errorf("Error running server: %v", err)
	}

	log.WithField("ctx", "main").Infof("Exiting main process")
}

////////////////// UTILITIES ///////////////////
const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// RandStringBytesRmndr returns a random string of n bytes
// Thanks to this Amazing stack-overflow answer:
// https://stackoverflow.com/questions/22892120/how-to-generate-a-random-string-of-a-fixed-length-in-golang
func RandStringBytesRmndr(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Int63()%int64(len(letterBytes))]
	}
	return string(b)
}
