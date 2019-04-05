package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Shopify/sarama"
	gh "github.com/google/go-github/v23/github"
	types "github.com/navikt/deployment/common/pkg/deployment"
	"github.com/navikt/deployment/common/pkg/payload"
	"github.com/navikt/deployment/hookd/pkg/github"
	"github.com/navikt/deployment/hookd/pkg/metrics"
)

type DeploymentHandler struct {
	Handler
	deploymentRequest *gh.DeploymentEvent
	repo              *gh.Repository
}

func (h *DeploymentHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h.prepare(w, r, h.unserialize, h.SecretToken); err != nil {
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
		Payload:    deployment.Payload,
		DeliveryID: h.deliveryID,
		Cluster:    deployment.GetEnvironment(),
		Timestamp:  time.Now().Unix(),
		Deadline:   time.Now().Add(time.Minute).Unix(),
	}, nil
}

func (h *DeploymentHandler) kafkaPublish(req *types.DeploymentRequest) error {
	payload, err := types.WrapMessage(req, h.KafkaClient.SignatureKey)
	if err != nil {
		return fmt.Errorf("while marshalling json: %s", err)
	}
	msg := sarama.ProducerMessage{
		Topic:     h.KafkaTopic,
		Value:     sarama.StringEncoder(payload),
		Timestamp: time.Unix(req.GetTimestamp(), 0),
	}
	_, _, err = h.KafkaClient.Producer.SendMessage(&msg)
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

func (h *DeploymentHandler) createAndLogDeploymentStatus(st *types.DeploymentStatus) error {
	status, _, err := github.CreateDeploymentStatus(h.GithubInstallationClient, st, h.Config.BaseURL)
	if err == nil {
		h.log.Infof("created GitHub deployment status %d in repository %s", status.GetID(), status.GetRepositoryURL())
	}
	return err
}

func (h *DeploymentHandler) addGithubStatusFailure(req *types.DeploymentRequest, err error) error {
	return h.createAndLogDeploymentStatus(&types.DeploymentStatus{
		Deployment:  req.GetDeployment(),
		DeliveryID:  req.GetDeliveryID(),
		State:       types.GithubDeploymentState_failure,
		Description: fmt.Sprintf("deployment request failed: %s", err),
	})
}

func (h *DeploymentHandler) addGithubStatusQueued(req *types.DeploymentRequest) error {
	return h.createAndLogDeploymentStatus(&types.DeploymentStatus{
		Deployment:  req.GetDeployment(),
		DeliveryID:  req.GetDeliveryID(),
		State:       types.GithubDeploymentState_queued,
		Description: "deployment request has been put on the queue for further processing",
	})
}

func (h *DeploymentHandler) validateTeamAccess() error {
	allowedTeams, err := h.TeamRepositoryStorage.Read(h.repo.GetFullName())
	if err != nil {
		return fmt.Errorf("unable to check if repository has team access: %s", err)
	}

	p := payload.Payload{}
	err = json.Unmarshal(h.deploymentRequest.GetDeployment().Payload, &p)
	if err != nil {
		return fmt.Errorf("decode error in deployment payload: %s", err)
	}

	for _, team := range allowedTeams {
		if p.Team == team {
			return nil
		}
	}

	return fmt.Errorf("the repository '%s' does not have access to deploy as team '%s'", h.repo.GetFullName(), p.Team)
}

func (h *DeploymentHandler) handler() (int, error) {
	if h.eventType != "deployment" {
		return http.StatusBadRequest, fmt.Errorf("unsupported event type %s", h.eventType)
	}

	metrics.WebhookRequests.Inc()

	if err := h.validateTeamAccess(); err != nil {
		return http.StatusForbidden, err
	}

	h.log.Infof("Dispatching deployment for %s", h.repo.GetFullName())

	deploymentRequest, err := h.kafkaPayload()
	if err != nil {
		return http.StatusInternalServerError, err
	}

	err = h.kafkaPublish(deploymentRequest)

	if err != nil {
		erro := h.addGithubStatusFailure(deploymentRequest, fmt.Errorf("unable to queue deployment request to Kafka"))
		if erro != nil {
			h.log.Errorf("unable to create Github deployment status: %s", erro)
		}
		return http.StatusInternalServerError, err
	}

	metrics.Dispatched.Inc()

	err = h.addGithubStatusQueued(deploymentRequest)

	if err != nil {
		h.log.Errorf("unable to create Github deployment status: %s", err)
	}

	return http.StatusCreated, nil
}
