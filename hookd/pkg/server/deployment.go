package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Shopify/sarama"
	"github.com/golang/protobuf/proto"
	gh "github.com/google/go-github/v23/github"
	types "github.com/navikt/deployment/common/pkg/deployment"
	"github.com/navikt/deployment/hookd/pkg/github"
	"net/http"
	"time"
)

type DeploymentHandler struct {
	Handler
	deploymentRequest *gh.DeploymentEvent
	repo              *gh.Repository
}

func (h *DeploymentHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h.prepare(w, r, h.unserialize, h.secretToken); err != nil {
		h.log.Error(err)
		return
	}
	h.finish(h.handler())
}

func (h *DeploymentHandler) kafkaPayload() (*types.DeploymentRequest, error) {
	owner, name, err := github.SplitFullname(h.repo.GetFullName())
	if err != nil {
		return nil, err
	}
	deployment := h.deploymentRequest.GetDeployment()
	if deployment == nil {
		return nil, fmt.Errorf("deployment object is empty")
	}
	return &types.DeploymentRequest{
		Deployment: &types.DeploymentSpec{
			Repository: &types.GithubRepository{
				Name:  name,
				Owner: owner,
			},
			DeploymentID: deployment.GetID(),
		},
		CorrelationID: h.deliveryID,
		Cluster:       deployment.GetEnvironment(),
		Timestamp:     time.Now().Unix(),
		Deadline:      time.Now().Add(time.Minute).Unix(),
	}, nil
}

func (h *DeploymentHandler) kafkaPublish(req *types.DeploymentRequest) error {
	payload, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("while marshalling json: %s", err)
	}
	msg := sarama.ProducerMessage{
		Topic: h.KafkaTopic,
		Value: sarama.StringEncoder(payload),
	}
	_, _, err = h.KafkaProducer.SendMessage(&msg)
	if err != nil {
		return fmt.Errorf("while publishing message to Kafka: %s", err)
	}
	return nil
}

func (h *DeploymentHandler) unserialize() error {
	h.deploymentRequest = &gh.DeploymentEvent{}
	if err := json.Unmarshal(h.data, h.deploymentRequest); err != nil {
		return err
	}
	h.repo = h.deploymentRequest.GetRepo()
	if h.repo == nil {
		return fmt.Errorf("deployment request doesn't specify repository")
	}
	return nil
}

func (h *DeploymentHandler) secretToken() (string, error) {
	deploymentRequest := gh.DeploymentEvent{}
	if err := json.Unmarshal(h.data, &deploymentRequest); err != nil {
		return "", err
	}
	repo := deploymentRequest.GetRepo()
	if repo == nil {
		return "", fmt.Errorf("deployment request doesn't specify repository")
	}
	secret, err := h.SecretClient.InstallationSecret(repo.GetFullName())
	if err != nil {
		return "", err
	}
	return secret.WebhookSecret, nil
}

func (h *DeploymentHandler) postFailure(deployment *types.DeploymentSpec, err error) error {
	return h.createDeploymentStatus(&types.DeploymentStatus{
		Deployment:  deployment,
		State:       types.GithubDeploymentState_failure,
		Description: fmt.Sprintf("deployment request failed: %s", err),
	})
}

func (h *DeploymentHandler) postSentToKafka(deployment *types.DeploymentSpec) error {
	return h.createDeploymentStatus(&types.DeploymentStatus{
		Deployment:  deployment,
		State:       types.GithubDeploymentState_queued,
		Description: "deployment request has been put on the queue for further processing",
	})
}

func (h *DeploymentHandler) createDeploymentStatus(m *types.DeploymentStatus) error {
	deployment := m.GetDeployment()
	if deployment == nil {
		return fmt.Errorf("empty deployment")
	}

	repo := deployment.GetRepository()
	if repo == nil {
		return fmt.Errorf("empty repository")
	}

	state := m.GetState().String()
	description := m.GetDescription()

	status, _, err := h.GithubInstallationClient.Repositories.CreateDeploymentStatus(
		context.Background(),
		repo.GetOwner(),
		repo.GetName(),
		deployment.GetDeploymentID(),
		&gh.DeploymentStatusRequest{
			State:       &state,
			Description: &description,
		},
	)

	if err == nil {
		h.log.Infof("created deployment status %d on repository %s/%s", status.GetID(), repo.GetOwner(), repo.GetName())
	}

	return err
}

func (h *DeploymentHandler) handler() (int, error) {
	if h.eventType != "deployment" {
		return http.StatusBadRequest, fmt.Errorf("unsupported event type %s", h.eventType)
	}

	h.log.Infof("Dispatching deployment for %s", h.repo.GetFullName())

	deploymentRequest, err := h.kafkaPayload()
	if err != nil {
		return http.StatusInternalServerError, err
	}

	err = h.kafkaPublish(deploymentRequest)

	if err != nil {
		erro := h.postFailure(deploymentRequest.Deployment, fmt.Errorf("unable to queue deployment request to Kafka"))
		if erro != nil {
			h.log.Errorf("unable to create Github deployment status: %s", erro)
		}
		return http.StatusInternalServerError, err
	}

	err = h.postSentToKafka(deploymentRequest.Deployment)

	if err != nil {
		h.log.Errorf("unable to create Github deployment status: %s", err)
	}

	return http.StatusCreated, nil
}