package prow

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"golang.org/x/exp/slices"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"k8s.io/klog/v2"
	v1 "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"sigs.k8s.io/yaml"
)

// NewArtifactScanner creates a new instance of ArtifactScanner,
// requires a valid ScannerConfig
func NewArtifactScanner(cfg ScannerConfig) (*ArtifactScanner, error) {
	ctx := context.Background()
	client, err := storage.NewClient(ctx, option.WithoutAuthentication())
	if err != nil {
		return nil, fmt.Errorf("failed to create new GCS client: %+v", err)
	}

	return &ArtifactScanner{
		Client: client,
		config: cfg,
	}, nil
}

// Run processes the artifacts associated with the Prow job and store required files
// with their associated openshift-ci step names and their content in ArtifactStepMap
func (as *ArtifactScanner) Run() error {
	pjYAML, err := getProwJobYAML(as.config.ProwJobID)
	if err != nil {
		return fmt.Errorf("failed to get prow job yaml: %+v", err)
	}

	jobTarget, err := determineJobTarget(pjYAML)
	if err != nil {
		return fmt.Errorf("failed to determine job target: %+v", err)
	}

	pjURL := pjYAML.Status.URL
	klog.Infof("got the prow job URL: %s", pjURL)
	// => e.g. [ "https://prow.ci.openshift.org/view/gs", "pr-logs/pull/redhat-appstudio_infra-deployments/123/pull-ci-redhat-appstudio-infra-deployments-main-appstudio-e2e-tests/123" ]
	sp := strings.Split(pjURL, "/"+bucketName+"/")
	if len(sp) != 2 {
		return fmt.Errorf("failed to determine object prefix - prow job url: '%s', bucket name: '%s'", pjURL, bucketName)
	}
	// => e.g. "pr-logs/pull/redhat-appstudio_infra-deployments/123/pull-ci-redhat-appstudio-infra-deployments-main-appstudio-e2e-tests/123/artifacts/appstudio-e2e-tests/"
	objectPrefix := sp[1] + "/artifacts/" + jobTarget + "/"
	as.ObjectPrefix = objectPrefix

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute*2)
	defer cancel()
	as.bucketHandle = as.Client.Bucket(bucketName)

	it := as.bucketHandle.Objects(ctx, &storage.Query{Prefix: objectPrefix})

	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to iterate over storage objects: %+v", err)
		}
		fullArtifactName := attrs.Name
		if as.isRequiredFile(fullArtifactName) {
			klog.Infof("found required file %s", fullArtifactName)
			// => e.g. [ "", "redhat-appstudio-e2e/artifacts/e2e-report.xml" ]
			sp := strings.Split(fullArtifactName, objectPrefix)
			if len(sp) != 2 {
				return fmt.Errorf("cannot determine filepath - object name: %s, object prefix: %s", fullArtifactName, objectPrefix)
			}
			parentStepFilePath := sp[1]

			// => e.g. [ "redhat-appstudio-e2e", "artifacts", "e2e-report.xml" ]
			sp = strings.Split(parentStepFilePath, "/")
			parentStepName := sp[0]

			if slices.Contains(as.config.StepsToSkip, parentStepName) {
				klog.Infof("skipping step name %s", parentStepName)
				continue
			}

			fileName := sp[len(sp)-1]

			rc, err := as.bucketHandle.Object(fullArtifactName).NewReader(ctx)
			if err != nil {
				return fmt.Errorf("failed to create objecthandle for %s: %+v", fullArtifactName, err)
			}
			data, err := io.ReadAll(rc)
			if err != nil {
				return fmt.Errorf("cannot read from storage reader: %+v", err)
			}

			artifact := Artifact{Content: string(data), FullName: fullArtifactName}

			// No artifact step map not initialized yet
			if as.ArtifactStepMap == nil {
				newArtifactMap := ArtifactFilenameMap{ArtifactFilename(fileName): artifact}
				as.ArtifactStepMap = map[ArtifactStepName]ArtifactFilenameMap{ArtifactStepName(parentStepName): newArtifactMap}
			} else {
				// Already have a record of an artifact being mapped to a step name
				if afMap, ok := as.ArtifactStepMap[ArtifactStepName(parentStepName)]; ok {
					afMap[ArtifactFilename(fileName)] = artifact
					as.ArtifactStepMap[ArtifactStepName(parentStepName)] = afMap
				} else { // Artifact map initialized, but the artifact filename does not belong to any collected step
					as.ArtifactStepMap[ArtifactStepName(parentStepName)] = ArtifactFilenameMap{ArtifactFilename(fileName): artifact}
				}
			}
		}
	}
	return nil
}

func (as *ArtifactScanner) isRequiredFile(fullArtifactName string) bool {
	return slices.ContainsFunc(as.config.FileNameFilter, func(s string) bool {
		re := regexp.MustCompile(s)
		return re.MatchString(fullArtifactName)
	})
}

func getProwJobYAML(jobID string) (*v1.ProwJob, error) {
	r, err := http.Get(prowJobYAMLPrefix + jobID)
	errTemplate := "failed to get prow job YAML:"
	if err != nil {
		return nil, fmt.Errorf("%s %s", errTemplate, err)
	}
	if r.StatusCode > 299 {
		return nil, fmt.Errorf("%s got response status code %v", errTemplate, r.StatusCode)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("%s %s", errTemplate, err)
	}
	var pj v1.ProwJob
	err = yaml.Unmarshal(body, &pj)
	if err != nil {
		return nil, fmt.Errorf("%s %s", errTemplate, err)
	}
	return &pj, nil
}

func determineJobTarget(pjYAML *v1.ProwJob) (jobTarget string, err error) {
	errPrefix := "failed to determine job target:"
	args := pjYAML.Spec.PodSpec.Containers[0].Args
	for _, arg := range args {
		if strings.Contains(arg, "--target") {
			sp := strings.Split(arg, "=")
			if len(sp) != 2 {
				return "", fmt.Errorf("%s expected %v to have len 2", errPrefix, sp)
			}
			jobTarget = sp[1]
			return
		}
	}
	return "", fmt.Errorf("%s expected %+v to contain arg --target", errPrefix, args)
}
