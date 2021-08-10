// Licensed to Apache Software Foundation (ASF) under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Apache Software Foundation (ASF) licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package injector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	annotationKeyAgentInjector    = "swck-agent-injected"
	annotationKeyAgentImage       = "swck-agent-image"
	annotationKeyServiceNameLabel = "swck-agent-service-name-label"
	annotationKeyOapServerAddress = "swck-agent-oap-server-address"
	annotationKeyInjectEnv        = "swck-agent-inject-env"
)

type Annotations struct {
	annotations map[string]string
}

func (a Annotations) getOrDefault(key, defaultV string) string {
	if value, ok := a.annotations[key]; ok {
		return value
	}
	return defaultV
}

// PodInjector injects agent into Pods
type PodInjector struct {
	Client  client.Client
	decoder *admission.Decoder
}

// PodInjector will process every coming pod under the
// specified namespace which labeled "swck-injection=enabled"
func (r *PodInjector) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	PodInjectorLog := logf.Log.WithName("PodInjector")
	err := r.decoder.Decode(req, pod)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	//if the pod don't have the label "swck-agent-injected=true",return ok
	if !needInject(pod) {
		PodInjectorLog.Info("don't inject agent")
		return admission.Allowed("ok")
	}

	//if the pod has the label "swck-agent-injected=true",add agent
	addAgent(pod)
	PodInjectorLog.Info("will inject agent,please wait for a moment!")

	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}

// PodInjector implements admission.DecoderInjector.
// A decoder will be automatically injected.

// InjectDecoder injects the decoder.
func (r *PodInjector) InjectDecoder(d *admission.Decoder) error {
	r.decoder = d
	return nil
}

//if pod has label "swck-agent-injected=true" , it means the pod needs agent injected
func needInject(pod *corev1.Pod) bool {
	injected := false

	labels := pod.ObjectMeta.Labels
	if labels == nil {
		return false
	}

	switch strings.ToLower(labels[annotationKeyAgentInjector]) {
	case "true":
		injected = true
	}

	return injected
}

//when agent is injected,we need to push agent image and
//  mount the volume to the specified directory
func addAgent(pod *corev1.Pod) {
	// set InitContainer's VolumeMount
	vm := corev1.VolumeMount{
		MountPath: "/sky/agent",
		Name:      "sky-agent",
	}

	annotations := Annotations{
		annotations: pod.ObjectMeta.Annotations,
	}

	// set the agent image to be injected
	needAddInitContainer := corev1.Container{
		Name:         "inject-sky-agent",
		Image:        annotations.getOrDefault(annotationKeyAgentImage, "apache/skywalking-java-agent:8.6.0-jdk8"),
		Command:      []string{"sh"},
		Args:         []string{"-c", "mkdir -p /sky/agent && cp -r /skywalking/agent/* /sky/agent"},
		VolumeMounts: []corev1.VolumeMount{vm},
	}

	// set emptyDir Volume
	needAddVolumes := corev1.Volume{
		Name:         "sky-agent",
		VolumeSource: corev1.VolumeSource{EmptyDir: nil},
	}

	// set container's VolumeMount
	needAddVolumeMount := corev1.VolumeMount{
		MountPath: "/sky/agent",
		Name:      "sky-agent",
	}

	// set container's EnvVar
	var needAddEnvs []corev1.EnvVar

	// agent opts
	agentOpts := corev1.EnvVar{
		Name:  annotations.getOrDefault(annotationKeyInjectEnv, "AGENT_OPTS"),
		Value: " -javaagent:/sky/agent/skywalking-agent.jar",
	}

	// service name
	var swAgentName string
	if label, ok := pod.ObjectMeta.Labels[annotations.getOrDefault(annotationKeyServiceNameLabel, "app")]; ok && label != "" {
		swAgentName = label
	} else {
		swAgentName = pod.Name
	}

	if swAgentName != "" {
		serviceName := corev1.EnvVar{
			Name:  "SW_AGENT_NAME",
			Value: swAgentName,
		}
		needAddEnvs = append(needAddEnvs, serviceName)
	}

	// oap server address
	oapServerAddress := corev1.EnvVar{
		Name:  "SW_AGENT_COLLECTOR_BACKEND_SERVICES",
		Value: annotations.getOrDefault(annotationKeyOapServerAddress, "127.0.0.1:11800"),
	}
	needAddEnvs = append(needAddEnvs, oapServerAddress)

	// add VolumeMount to spec
	if pod.Spec.Volumes != nil {
		pod.Spec.Volumes = append(pod.Spec.Volumes, needAddVolumes)
	} else {
		pod.Spec.Volumes = []corev1.Volume{needAddVolumes}
	}

	// add InitContainers to spec
	if pod.Spec.InitContainers != nil {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, needAddInitContainer)
	} else {
		pod.Spec.InitContainers = []corev1.Container{needAddInitContainer}
	}

	// add VolumeMount and env to every container
	for i := 0; i < len(pod.Spec.Containers); i++ {
		pod.Spec.Containers[i].VolumeMounts = append(pod.Spec.Containers[i].VolumeMounts, needAddVolumeMount)
		pod.Spec.Containers[i].Env = append(pod.Spec.Containers[i].Env, needAddEnvs...)

		// append if the env variable already exist
		alreadyExist := false
		envs := pod.Spec.Containers[i].Env
		for ii := 0; ii < len(envs); ii++ {
			env := envs[ii]
			if env.Name == agentOpts.Name {
				env.Value = fmt.Sprintf("%s %s", env.Value, agentOpts.Value)
				envs[ii] = env
				alreadyExist = true
				break
			}
		}

		if !alreadyExist {
			pod.Spec.Containers[i].Env = append(pod.Spec.Containers[i].Env, agentOpts)
		}
	}
}
