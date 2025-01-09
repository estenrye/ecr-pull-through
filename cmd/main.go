package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	v1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func handleRoot(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "ECR Pull-through webhook %q", html.EscapeString(r.URL.Path))
}

var config *Config

func generatePatch(registryList []string, specKey string, containerIndex int, awsAccountId string, awsRegion string, containerImage string, podNamespace string, podGeneratedName string) (bool, map[string]string) {
	ecrRegistryHostname := fmt.Sprintf("%s.dkr.ecr.%s.amazonaws.com", awsAccountId, awsRegion)
	dockerHubPullThroughCacheConfigured := false

	// shortcut to avoid patching images that are already patched.
	if strings.HasPrefix(containerImage, ecrRegistryHostname) {
		log.Printf("{ \"appliedPatch\": false, \"podNamespace\": \"%s\", \"podGeneratedName\": \"%s\", \"specKey\": \"%s\", \"index\": %d, \"originalImage\": \"%s\" }",
			podNamespace, podGeneratedName, specKey, containerIndex, containerImage)
		return false, nil
	}

	ecrRegex := regexp.MustCompile(`.+\.dkr\.ecr\..+\.amazonaws\.com/`)
	if ecrRegex.MatchString(containerImage) {
		log.Printf("{ \"appliedPatch\": false, \"podNamespace\": \"%s\", \"podGeneratedName\": \"%s\", \"specKey\": \"%s\", \"index\": %d, \"originalImage\": \"%s\" }",
			podNamespace, podGeneratedName, specKey, containerIndex, containerImage)
		return false, nil
	}

	// Loop through the list of configured pull-through cache registries.
	// If the image contains a registry prefix, patch it with the ECR pull through cache image name.
	for _, registry := range registryList {

		// Note for later whether the docker.io registry is in the list of configured registries
		// This value is used to trigger docker.io specific logic if we exit this loop without
		// patching the image.
		if registry == "docker.io" {
			dockerHubPullThroughCacheConfigured = true
		}

		if strings.HasPrefix(containerImage, registry) {
			newImage := fmt.Sprintf("%s/%s", ecrRegistryHostname, containerImage)

			// split containerImage to find out if it has two or three parts.
			// if it has three parts, like docker.io/foo/bar, then it is not a library image.
			// if it has two parts, like docker.io/bar, then it is a library image and needs library injected into the path.
			parts := strings.Split(containerImage, "/")
			if len(parts) == 2 {
				newImage = fmt.Sprintf("%s/%s/library/%s", ecrRegistryHostname, parts[0], parts[1])
			}

			log.Printf("{ \"appliedPatch\": true, \"podNamespace\": \"%s\", \"podGeneratedName\": \"%s\", \"specKey\": \"%s\", \"index\": %d, \"originalImage\": \"%s\", \"newImage\": \"%s\" }",
				podNamespace, podGeneratedName, specKey, containerIndex, containerImage, newImage)

			return true, map[string]string{
				"op":    "replace",
				"path":  fmt.Sprintf("/spec/%s/%d/image", specKey, containerIndex),
				"value": newImage,
			}
		}
	}

	// At this point, the image has not been previously treated by the controller
	// and does not contain any registry prefixes that have been defined in the configMap.
	// We also know whether the docker.io registry is in the list of configured registries.
	// We need to check if the image is a library image or not.

	if dockerHubPullThroughCacheConfigured {
		parts := strings.Split(containerImage, "/")
		// This logic handles library images without a registry prefix.
		if len(parts) == 1 {
			newImage := fmt.Sprintf("%s/docker.io/library/%s", ecrRegistryHostname, containerImage)

			log.Printf("{ \"appliedPatch\": true, \"podNamespace\": \"%s\", \"podGeneratedName\": \"%s\", \"specKey\": \"%s\", \"index\": %d, \"originalImage\": \"%s\", \"newImage\": \"%s\" }",
				podNamespace, podGeneratedName, specKey, containerIndex, containerImage, newImage)

			return true, map[string]string{
				"op":    "replace",
				"path":  fmt.Sprintf("/spec/%s/%d/image", specKey, containerIndex),
				"value": newImage,
			}
		}

		// this logic handles non-library images without a registry prefix.
		if len(parts) == 2 {
			newImage := fmt.Sprintf("%s/docker.io/%s", ecrRegistryHostname, containerImage)

			log.Printf("{ \"appliedPatch\": true, \"podNamespace\": \"%s\", \"podGeneratedName\": \"%s\", \"specKey\": \"%s\", \"index\": %d, \"originalImage\": \"%s\", \"newImage\": \"%s\" }",
				podNamespace, podGeneratedName, specKey, containerIndex, containerImage, newImage)

			return true, map[string]string{
				"op":    "replace",
				"path":  fmt.Sprintf("/spec/%s/%d/image", specKey, containerIndex),
				"value": newImage,
			}
		}
	}

	// The pod will not be patched if the code reaches this point.
	log.Printf("{ \"appliedPatch\": false, \"podNamespace\": \"%s\", \"podGeneratedName\": \"%s\", \"specKey\": \"%s\", \"index\": %d, \"originalImage\": \"%s\" }",
		podNamespace, podGeneratedName, specKey, containerIndex, containerImage)
	return false, nil
}

func handleMutate(w http.ResponseWriter, r *http.Request) {

	// read the body / request
	body, err := io.ReadAll(r.Body)
	defer r.Body.Close()
	if err != nil {
		log.Printf("{ \"state\": \"error\", msg: \"%s\" }", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "%s", err)
	}

	// mutate the request
	mutated, err := actuallyMutate(body)
	if err != nil {
		log.Printf("{ \"state\": \"error\", msg: \"%s\" }", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "%s", err)
	}

	// and write it back
	w.WriteHeader(http.StatusOK)
	w.Write(mutated)
}

func actuallyMutate(body []byte) ([]byte, error) {
	// unmarshal request into AdmissionReview struct
	admReview := v1beta1.AdmissionReview{}
	if err := json.Unmarshal(body, &admReview); err != nil {
		return nil, fmt.Errorf("unmarshaling request failed with %s", err)
	}

	var err error
	var pod *corev1.Pod

	responseBody := []byte{}
	ar := admReview.Request
	resp := v1beta1.AdmissionResponse{}

	if ar != nil {

		// get the Pod object and unmarshal it into its struct, if we cannot, we might as well stop here
		if err := json.Unmarshal(ar.Object.Raw, &pod); err != nil {
			return nil, fmt.Errorf("unable unmarshal pod json object %v", err)
		}
		log.Printf("{ \"podNamespace\": \"%s\", \"podGeneratedName\": \"%s\", \"state\": \"started\", msg: \"\" }", pod.Namespace, pod.ObjectMeta.GenerateName)
		// set response options
		resp.Allowed = true
		resp.UID = ar.UID
		pT := v1beta1.PatchTypeJSONPatch
		resp.PatchType = &pT

		// the actual mutation is done by a string in JSONPatch style, i.e. we don't _actually_ modify the object, but
		// tell K8S how it should modifiy it
		p := []map[string]string{}
		// Containers
		for i, container := range pod.Spec.Containers {
			patchApplied, patch := generatePatch(config.RegistryList(), "containers", i, config.AwsAccountID, config.AwsRegion, container.Image, pod.Namespace, pod.ObjectMeta.GenerateName)
			if patchApplied {
				p = append(p, patch)
			}
		}

		// InitContainers
		for i, initcontainer := range pod.Spec.InitContainers {
			patchApplied, patch := generatePatch(config.RegistryList(), "initContainers", i, config.AwsAccountID, config.AwsRegion, initcontainer.Image, pod.Namespace, pod.ObjectMeta.GenerateName)
			if patchApplied {
				p = append(p, patch)
			}
		}

		// EphemeralContainers
		for i, ephemeralcontainer := range pod.Spec.EphemeralContainers {
			patchApplied, patch := generatePatch(config.RegistryList(), "ephemeralContainers", i, config.AwsAccountID, config.AwsRegion, ephemeralcontainer.Image, pod.Namespace, pod.ObjectMeta.GenerateName)
			if patchApplied {
				p = append(p, patch)
			}
		}

		// parse the []map into JSON
		resp.Patch, _ = json.Marshal(p)

		// Success, of course ;)
		resp.Result = &metav1.Status{
			Status: "Success",
		}

		admReview.Response = &resp
		// back into JSON so we can return the finished AdmissionReview w/ Response directly
		// w/o needing to convert things in the http handler
		responseBody, err = json.Marshal(admReview)

		if err != nil {
			return nil, err // untested section
		}
		log.Printf("{ \"podNamespace\": \"%s\", \"podGeneratedName\": \"%s\", \"state\": \"successful\" }", pod.Namespace, pod.ObjectMeta.GenerateName)
	}

	return responseBody, nil
}

func main() {
	var err error
	config, err = ReadConf("/conf/registries.yaml")
	if err != nil {
		log.Fatalf("Failed to read config: %v", err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", handleRoot)
	mux.HandleFunc("/mutate", handleMutate)

	s := &http.Server{
		Addr:           ":8443",
		Handler:        mux,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1048576
	}

	// Check for TLS certificate and key files
	_, certErr := os.Stat("/tls/tls.crt")
	_, keyErr := os.Stat("/tls/tls.key")

	if os.IsNotExist(certErr) || os.IsNotExist(keyErr) {
		log.Println("Starting server without TLS...")
		log.Fatal(s.ListenAndServe())
	} else {
		log.Println("Starting server with TLS...")
		log.Fatal(s.ListenAndServeTLS("/tls/tls.crt", "/tls/tls.key"))
	}
}
