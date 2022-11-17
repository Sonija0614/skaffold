/*
Copyright 2022 The Skaffold Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloudrun

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/api/run/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "sigs.k8s.io/yaml"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/access"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/config"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/debug"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/deploy/label"
	sErrors "github.com/GoogleContainerTools/skaffold/pkg/skaffold/errors"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/gcp"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/graph"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/kubernetes/manifest"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/log"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/output"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/latest"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/status"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/sync"
	"github.com/GoogleContainerTools/skaffold/proto/v1"
)

// Config contains config options needed for cloud run
type Config interface {
	PortForwardResources() []*latest.PortForwardResource
	PortForwardOptions() config.PortForwardOptions
	Mode() config.RunMode
	Tail() bool
}

// Deployer deploys code to Google Cloud Run.
type Deployer struct {
	configName string
	logger     *LogAggregator
	accessor   *RunAccessor
	monitor    *Monitor
	labeller   *label.DefaultLabeller

	Project string
	Region  string

	// additional client options for connecting to Cloud Run, used for tests
	clientOptions []option.ClientOption
	useGcpOptions bool
}

// NewDeployer creates a new Deployer for Cloud Run from the Skaffold deploy config.
func NewDeployer(cfg Config, labeller *label.DefaultLabeller, crDeploy *latest.CloudRunDeploy, configName string) (*Deployer, error) {
	return &Deployer{
		configName: configName,
		Project:    crDeploy.ProjectID,
		Region:     crDeploy.Region,
		// TODO: implement logger for Cloud Run.
		logger:        NewLoggerAggregator(cfg, labeller.GetRunID()),
		accessor:      NewAccessor(cfg, labeller.GetRunID()),
		labeller:      labeller,
		useGcpOptions: true,
	}, nil
}

// Deploy creates a Cloud Run service using the provided manifest.
func (d *Deployer) Deploy(ctx context.Context, out io.Writer, artifacts []graph.Artifact, manifestsByConfig manifest.ManifestListByConfig) error {
	manifests := manifestsByConfig.GetForConfig(d.ConfigName())

	for _, manifest := range manifests {
		if err := d.deployToCloudRun(ctx, out, manifest); err != nil {
			return err
		}
	}
	return nil
}

func (d *Deployer) ConfigName() string {
	return d.configName
}

// Dependencies list the files that would trigger a redeploy
func (d *Deployer) Dependencies() ([]string, error) {
	return []string{}, nil
}

// Cleanup deletes the created Cloud Run services
func (d *Deployer) Cleanup(ctx context.Context, out io.Writer, dryRun bool, byConfig manifest.ManifestListByConfig) error {
	return d.deleteRunService(ctx, out, dryRun, byConfig.GetForConfig(d.configName))
}

// GetDebugger Get the Debugger for Cloud Run. Not supported by this deployer.
func (d *Deployer) GetDebugger() debug.Debugger {
	return &debug.NoopDebugger{}
}

// GetLogger Get the logger for the Cloud Run deploy.
func (d *Deployer) GetLogger() log.Logger {
	return d.logger
}

// GetAccessor gets a no-op accessor for Cloud Run.
func (d *Deployer) GetAccessor() access.Accessor {
	return d.accessor
}

// GetSyncer gets the file syncer for Cloud Run. Not supported by this deployer.
func (d *Deployer) GetSyncer() sync.Syncer {
	return &sync.NoopSyncer{}
}

// TrackBuildArtifacts is not supported by this deployer.
func (d *Deployer) TrackBuildArtifacts(_, _ []graph.Artifact) {

}

// RegisterLocalImages is not supported by this deployer.
func (d *Deployer) RegisterLocalImages([]graph.Artifact) {

}

// GetStatusMonitor gets the resource that will monitor deployment status.
func (d *Deployer) GetStatusMonitor() status.Monitor {
	return d.getMonitor()
}

func (d *Deployer) getMonitor() *Monitor {
	if d.monitor == nil {
		d.monitor = NewMonitor(d.labeller, d.clientOptions)
	}
	return d.monitor
}
func (d *Deployer) deployToCloudRun(ctx context.Context, out io.Writer, manifest []byte) error {
	cOptions := d.clientOptions
	if d.useGcpOptions {
		cOptions = append(cOptions, option.WithEndpoint(fmt.Sprintf("%s-run.googleapis.com", d.Region)))
		cOptions = append(gcp.ClientOptions(ctx), cOptions...)
	}
	crclient, err := run.NewService(ctx, cOptions...)
	if err != nil {
		return sErrors.NewError(fmt.Errorf("unable to create Cloud Run Client"), &proto.ActionableErr{
			Message: err.Error(),
			ErrCode: proto.StatusCode_DEPLOY_GET_CLOUD_RUN_CLIENT_ERR,
		})
	}
	// figure out which type we have:
	resource := &unstructured.Unstructured{}
	if err = k8syaml.Unmarshal(manifest, resource); err != nil {
		return sErrors.NewError(fmt.Errorf("unable to unmarshal Cloud Run Service config"), &proto.ActionableErr{
			Message: err.Error(),
			ErrCode: proto.StatusCode_DEPLOY_READ_MANIFEST_ERR,
		})
	}
	var resName *RunResourceName
	switch {
	case resource.GetAPIVersion() == "serving.knative.dev/v1" && resource.GetKind() == "Service":
		resName, err = d.deployService(crclient, manifest, out)
		// the accessor only supports services. Jobs don't run by themselves so port forwarding doesn't make sense.
		if resName != nil {
			d.accessor.AddResource(*resName)
		}
	case resource.GetAPIVersion() == "run.googleapis.com/v1" && resource.GetKind() == "Job":
		resName, err = d.deployJob(crclient, manifest, out)
	default:
		err = sErrors.NewError(fmt.Errorf("unsupported Kind for Cloud Run Deployer: %s/%s", resource.GetAPIVersion(), resource.GetKind()),
			&proto.ActionableErr{
				Message: "Kind is not supported",
				ErrCode: proto.StatusCode_DEPLOY_READ_MANIFEST_ERR,
			})
	}

	if err != nil {
		return err
	}

	d.getMonitor().Resources = append(d.getMonitor().Resources, *resName)
	return nil
}

func (d *Deployer) deployService(crclient *run.APIService, manifest []byte, out io.Writer) (*RunResourceName, error) {
	service := &run.Service{}
	if err := k8syaml.Unmarshal(manifest, service); err != nil {
		return nil, sErrors.NewError(fmt.Errorf("unable to unmarshal Cloud Run Service config"), &proto.ActionableErr{
			Message: err.Error(),
			ErrCode: proto.StatusCode_DEPLOY_READ_MANIFEST_ERR,
		})
	}
	if d.Project != "" {
		service.Metadata.Namespace = d.Project
	} else if service.Metadata.Namespace == "" {
		return nil, sErrors.NewError(fmt.Errorf("unable to detect project for Cloud Run"), &proto.ActionableErr{
			Message: "No Google Cloud project found in Cloud Run Manifest or Skaffold Config",
			ErrCode: proto.StatusCode_DEPLOY_READ_MANIFEST_ERR,
		})
	}
	// we need to strip "skaffold.dev" from the run-id label because gcp labels don't support domains
	runID, foundID := service.Metadata.Labels["skaffold.dev/run-id"]
	if foundID {
		delete(service.Metadata.Labels, "skaffold.dev/run-id")
		service.Metadata.Labels["run-id"] = runID
	}
	if service.Spec != nil && service.Spec.Template != nil && service.Spec.Template.Metadata != nil {
		runID, foundID = service.Spec.Template.Metadata.Labels["skaffold.dev/run-id"]
		if foundID {
			delete(service.Spec.Template.Metadata.Labels, "skaffold.dev/run-id")
			service.Spec.Template.Metadata.Labels["run-id"] = runID
		}
	}
	resName := RunResourceName{
		Project: service.Metadata.Namespace,
		Region:  d.Region,
		Service: service.Metadata.Name,
	}
	output.Default.Fprintln(out, "Deploying Cloud Run service:\n\t", service.Metadata.Name)
	parent := fmt.Sprintf("projects/%s/locations/%s", service.Metadata.Namespace, d.Region)

	sName := resName.String()
	d.logger.AddResource(resName)
	getCall := crclient.Projects.Locations.Services.Get(sName)
	_, err := getCall.Do()

	if err != nil {
		gErr, ok := err.(*googleapi.Error)
		if !ok || gErr.Code != http.StatusNotFound {
			return nil, sErrors.NewError(fmt.Errorf("error checking Cloud Run State: %w", err), &proto.ActionableErr{
				Message: err.Error(),
				ErrCode: proto.StatusCode_DEPLOY_CLOUD_RUN_GET_SERVICE_ERR,
			})
		}
		// This is a new service, we need to create it
		createCall := crclient.Projects.Locations.Services.Create(parent, service)
		_, err = createCall.Do()
	} else {
		replaceCall := crclient.Projects.Locations.Services.ReplaceService(sName, service)
		_, err = replaceCall.Do()
	}
	if err != nil {
		return nil, sErrors.NewError(fmt.Errorf("error deploying Cloud Run Service: %s", err), &proto.ActionableErr{
			Message: err.Error(),
			ErrCode: proto.StatusCode_DEPLOY_CLOUD_RUN_UPDATE_SERVICE_ERR,
		})
	}
	return &resName, nil
}

func (d *Deployer) deployJob(crclient *run.APIService, manifest []byte, out io.Writer) (*RunResourceName, error) {
	job := &run.Job{}
	if err := k8syaml.Unmarshal(manifest, job); err != nil {
		return nil, sErrors.NewError(fmt.Errorf("unable to unmarshal Cloud Run Service config"), &proto.ActionableErr{
			Message: err.Error(),
			ErrCode: proto.StatusCode_DEPLOY_READ_MANIFEST_ERR,
		})
	}
	if d.Project != "" {
		job.Metadata.Namespace = d.Project
	} else if job.Metadata.Namespace == "" {
		return nil, sErrors.NewError(fmt.Errorf("unable to detect project for Cloud Run"), &proto.ActionableErr{
			Message: "No Google Cloud project found in Cloud Run Manifest or Skaffold Config",
			ErrCode: proto.StatusCode_DEPLOY_READ_MANIFEST_ERR,
		})
	}
	// we need to strip "skaffold.dev" from the run-id label because gcp labels don't support domains
	runID, foundID := job.Metadata.Labels["skaffold.dev/run-id"]
	if foundID {
		delete(job.Metadata.Labels, "skaffold.dev/run-id")
		job.Metadata.Labels["run-id"] = runID
	}
	if job.Spec != nil && job.Spec.Template != nil && job.Spec.Template.Metadata != nil {
		runID, foundID = job.Spec.Template.Metadata.Labels["skaffold.dev/run-id"]
		if foundID {
			delete(job.Spec.Template.Metadata.Labels, "skaffold.dev/run-id")
			job.Spec.Template.Metadata.Labels["run-id"] = runID
		}
	}
	resName := RunResourceName{
		Project: job.Metadata.Namespace,
		Region:  d.Region,
		Job:     job.Metadata.Name,
	}
	output.Default.Fprintln(out, "Deploying Cloud Run service:\n\t", job.Metadata.Name)
	parent := fmt.Sprintf("namespaces/%s", job.Metadata.Namespace)

	sName := resName.String()
	getCall := crclient.Namespaces.Jobs.Get(sName)
	_, err := getCall.Do()

	if err != nil {
		gErr, ok := err.(*googleapi.Error)
		if !ok || gErr.Code != http.StatusNotFound {
			return nil, sErrors.NewError(fmt.Errorf("error checking Cloud Run State: %w", err), &proto.ActionableErr{
				Message: err.Error(),
				ErrCode: proto.StatusCode_DEPLOY_CLOUD_RUN_GET_SERVICE_ERR,
			})
		}
		// This is a new service, we need to create it
		createCall := crclient.Namespaces.Jobs.Create(parent, job)
		_, err = createCall.Do()
	} else {
		replaceCall := crclient.Namespaces.Jobs.ReplaceJob(sName, job)
		_, err = replaceCall.Do()
	}
	if err != nil {
		return nil, sErrors.NewError(fmt.Errorf("error deploying Cloud Run Job: %s", err), &proto.ActionableErr{
			Message: err.Error(),
			ErrCode: proto.StatusCode_DEPLOY_CLOUD_RUN_UPDATE_SERVICE_ERR,
		})
	}
	return &resName, nil
}

func (d *Deployer) deleteRunService(ctx context.Context, out io.Writer, dryRun bool, manifests manifest.ManifestList) error {
	if len(manifests) != 1 {
		return sErrors.NewError(fmt.Errorf("unexpected manifest for Cloud Run"),
			&proto.ActionableErr{
				Message: "Cloud Run expected a single Service manifest.",
				ErrCode: proto.StatusCode_DEPLOY_READ_MANIFEST_ERR,
			})
	}
	service := &run.Service{}
	if err := k8syaml.Unmarshal(manifests[0], service); err != nil {
		return sErrors.NewError(fmt.Errorf("unable to unmarshal Cloud Run Service config"), &proto.ActionableErr{
			Message: err.Error(),
			ErrCode: proto.StatusCode_DEPLOY_READ_MANIFEST_ERR,
		})
	}

	var projectID string
	switch {
	case d.Project != "":
		projectID = d.Project
	case service.Metadata.Namespace != "":
		projectID = service.Metadata.Namespace
	default:
		// no project specified, we don't know what to delete.
		return sErrors.NewError(fmt.Errorf("unable to determine Google Cloud Project"), &proto.ActionableErr{
			Message: "No Google Cloud Project found in Cloud Run manifest or Skaffold Manifest.",
			ErrCode: proto.StatusCode_DEPLOY_READ_MANIFEST_ERR,
		})
	}
	parent := fmt.Sprintf("projects/%s/locations/%s", projectID, d.Region)
	sName := fmt.Sprintf("%s/services/%s", parent, service.Metadata.Name)
	if dryRun {
		output.Yellow.Fprintln(out, sName)
		return nil
	}
	crclient, err := run.NewService(ctx, append(gcp.ClientOptions(ctx), d.clientOptions...)...)
	if err != nil {
		return sErrors.NewError(fmt.Errorf("unable to create Cloud Run Client"), &proto.ActionableErr{
			Message: err.Error(),
			ErrCode: proto.StatusCode_DEPLOY_GET_CLOUD_RUN_CLIENT_ERR,
		})
	}
	delCall := crclient.Projects.Locations.Services.Delete(sName)
	_, err = delCall.Do()
	if err != nil {
		return sErrors.NewError(fmt.Errorf("unable to delete Cloud Run Service"), &proto.ActionableErr{
			Message: err.Error(),
			ErrCode: proto.StatusCode_DEPLOY_CLOUD_RUN_DELETE_SERVICE_ERR,
		})
	}
	return nil
}
