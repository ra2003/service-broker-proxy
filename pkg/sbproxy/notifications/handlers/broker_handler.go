package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Peripli/service-broker-proxy/pkg/sm"
	"github.com/Peripli/service-broker-proxy/pkg/util"

	"github.com/Peripli/service-manager/pkg/util/slice"

	"github.com/pkg/errors"

	"github.com/Peripli/service-manager/storage/interceptors"

	"github.com/Peripli/service-manager/pkg/log"

	"github.com/Peripli/service-manager/pkg/types"

	"github.com/Peripli/service-broker-proxy/pkg/platform"
)

type brokerPayload struct {
	New brokerWithAdditionalDetails `json:"new"`
	Old brokerWithAdditionalDetails `json:"old"`
}

type brokerWithAdditionalDetails struct {
	Resource   *types.ServiceBroker          `json:"resource"`
	Additional interceptors.BrokerAdditional `json:"additional"`
}

// Validate validates the broker payload
func (bp brokerPayload) Validate(op types.NotificationOperation) error {
	switch op {
	case types.CREATED:
		if err := bp.New.Validate(); err != nil {
			return err
		}
	case types.MODIFIED:
		if err := bp.Old.Validate(); err != nil {
			return err
		}
		if err := bp.New.Validate(); err != nil {
			return err
		}
	case types.DELETED:
		if err := bp.Old.Validate(); err != nil {
			return err
		}
	}

	return nil
}

// Validate validates the broker and its additional details
func (bad brokerWithAdditionalDetails) Validate() error {
	if bad.Resource == nil {
		return fmt.Errorf("resource in notification payload cannot be nil")
	}
	if bad.Resource.ID == "" {
		return fmt.Errorf("broker ID cannot be empty")
	}
	if bad.Resource.BrokerURL == "" {
		return fmt.Errorf("broker URL cannot be empty")
	}
	if bad.Resource.Name == "" {
		return fmt.Errorf("broker name cannot be empty")
	}

	return bad.Additional.Validate()
}

// BrokerResourceNotificationsHandler handles notifications for brokers
type BrokerResourceNotificationsHandler struct {
	SMClient       sm.Client
	BrokerClient   platform.BrokerClient
	CatalogFetcher platform.CatalogFetcher

	ProxyPrefix string
	SMPath      string

	BrokerBlacklist []string
	TakeoverEnabled bool
}

// OnCreate creates brokers from the specified notification payload by invoking the proper platform clients
func (bnh *BrokerResourceNotificationsHandler) OnCreate(ctx context.Context, notification *types.Notification) {
	payload := notification.Payload
	log.C(ctx).Debugf("Processing broker create notification with payload %s...", string(payload))

	brokerPayload, err := bnh.unmarshalPayload(types.CREATED, payload)
	if err != nil {
		log.C(ctx).WithError(err).Error("could not extract broker payload")
		return
	}

	brokerToCreate := brokerPayload.New
	brokerProxyPath := bnh.brokerProxyPath(brokerToCreate.Resource)
	brokerProxyName := bnh.brokerProxyName(brokerToCreate.Resource)

	if slice.StringsAnyEquals(bnh.BrokerBlacklist, brokerToCreate.Resource.Name) {
		log.C(ctx).Infof("Broker name %s for broker create notification is part of broker blacklist. Skipping notification...", brokerToCreate.Resource.Name)
		return
	}

	log.C(ctx).Infof("Attempting to find platform broker with name %s in platform...", brokerToCreate.Resource.Name)

	existingBroker, err := bnh.BrokerClient.GetBrokerByName(ctx, brokerToCreate.Resource.Name)
	if err != nil {
		log.C(ctx).Debugf("Could not find platform broker in platform with name %s: %s", brokerToCreate.Resource.Name, err)
	}

	username, password, passwordHash, err := util.GenerateBrokerPlatformCredentials()
	if err != nil {
		log.C(ctx).Debugf("Could not generate broker platform credentials for broker (%s): %s", brokerToCreate.Resource.Name, err)
		return
	}

	credentials := &types.BrokerPlatformCredential{
		Username:       username,
		PasswordHash:   passwordHash,
		BrokerID:       brokerToCreate.Resource.ID,
		NotificationID: notification.ID,
	}

	if existingBroker == nil {
		log.C(ctx).Infof("Could not find platform broker in platform with name %s. Attempting to create a SM proxy registration...", brokerProxyName)

		credentialResponse, err := bnh.SMClient.PutCredentials(ctx, credentials)
		if err != nil {
			log.C(ctx).Debugf("Could not register broker platform credentials for broker (%s): %s", brokerToCreate.Resource.Name, err)
			return
		}

		createRequest := &platform.CreateServiceBrokerRequest{
			ID:        brokerToCreate.Resource.GetID(),
			Name:      brokerProxyName,
			BrokerURL: brokerProxyPath,
			Username:  username,
			Password:  password,
		}
		broker, err := bnh.BrokerClient.CreateBroker(ctx, createRequest)
		if err != nil {
			log.C(ctx).WithError(err).Errorf("error creating broker with name %s and URL %s", createRequest.Name, createRequest.BrokerURL)
			return
		}
		log.C(ctx).Infof("Successfully created SM proxy registration in platform for broker with name %s", brokerProxyName)
		bnh.resetBrokerCache(ctx, nil, broker)
		bnh.activateCredentials(ctx, credentialResponse)
	} else {
		log.C(ctx).Infof("Successfully found broker in platform with name %s and URL %s. Checking if takeover is needed...", existingBroker.Name, existingBroker.BrokerURL)
		if shouldBeTakenOver(existingBroker, brokerToCreate.Resource) {
			if !bnh.TakeoverEnabled {
				log.C(ctx).Infof("Broker %s is eligible for taking over, but broker takeover is disabled. Skipping notification...", existingBroker.Name)
				return
			}

			credentialResponse, err := bnh.SMClient.PutCredentials(ctx, credentials)
			if err != nil {
				log.C(ctx).Debugf("Could not update broker platform credentials for broker (%s): %s", brokerToCreate.Resource.Name, err)
				return
			}

			updateRequest := &platform.UpdateServiceBrokerRequest{
				ID:        brokerToCreate.Resource.ID,
				GUID:      existingBroker.GUID,
				Name:      brokerProxyName,
				BrokerURL: brokerProxyPath,
				Username:  username,
				Password:  password,
			}

			log.C(ctx).Infof("Taking over platform broker with name %s and URL %s...", existingBroker.Name, existingBroker.BrokerURL)
			newBroker, err := bnh.BrokerClient.UpdateBroker(ctx, updateRequest)
			if err != nil {
				log.C(ctx).WithError(err).Errorf("error taking over platform broker with GUID %s with SM broker with id %s", existingBroker.GUID, brokerToCreate.Resource.GetID())
				return
			}
			bnh.resetBrokerCache(ctx, existingBroker, newBroker)
			bnh.activateCredentials(ctx, credentialResponse)
		} else {
			log.C(ctx).Errorf("conflict error: existing platform broker with name %s and URL %s CANNOT be taken over as SM broker with URL %s. The URLs need to be the same", existingBroker.Name, existingBroker.BrokerURL, brokerToCreate.Resource.BrokerURL)
		}
	}
}

// OnUpdate modifies brokers from the specified notification payload by invoking the proper platform clients
func (bnh *BrokerResourceNotificationsHandler) OnUpdate(ctx context.Context, notification *types.Notification) {
	payload := notification.Payload
	log.C(ctx).Debugf("Processing broker update notification with payload %s...", string(payload))

	brokerPayload, err := bnh.unmarshalPayload(types.MODIFIED, payload)
	if err != nil {
		log.C(ctx).WithError(err).Error("could not extract broker payload")
		return
	}

	brokerBeforeUpdate := brokerPayload.Old
	brokerAfterUpdate := brokerPayload.New
	brokerProxyNameBefore := bnh.brokerProxyName(brokerBeforeUpdate.Resource)
	brokerProxyNameAfter := bnh.brokerProxyName(brokerAfterUpdate.Resource)
	brokerProxyPath := bnh.brokerProxyPath(brokerAfterUpdate.Resource)

	brokerToFind := determineBrokerNameToFind(brokerProxyNameBefore, brokerProxyNameAfter)

	if slice.StringsAnyEquals(bnh.BrokerBlacklist, brokerBeforeUpdate.Resource.Name) {
		log.C(ctx).Infof("Broker name %s for broker update notification is part of broker blacklist. Skipping notification...", brokerBeforeUpdate.Resource.Name)
		return
	}

	log.C(ctx).Infof("Attempting to find platform broker with name %s in platform...", brokerToFind)
	existingBroker, err := bnh.BrokerClient.GetBrokerByName(ctx, brokerToFind)
	if err != nil {
		log.C(ctx).Errorf("Could not find broker with name %s in the platform: %s. No update will be attempted", brokerToFind, err)
		return
	} else if existingBroker == nil {
		log.C(ctx).Errorf("Could not find broker with name %s in the platform. No update will be attempted", brokerToFind)
		return
	}
	log.C(ctx).Infof("Successfully found platform broker with name %s and URL %s.", existingBroker.Name, existingBroker.BrokerURL)

	if existingBroker.BrokerURL != brokerProxyPath {
		log.C(ctx).Errorf("Platform broker with name %s has an URL %s and is not taken over by SM. No update will be attempted", existingBroker.Name, existingBroker.BrokerURL)
		return
	}

	username, password, passwordHash, err := util.GenerateBrokerPlatformCredentials()
	if err != nil {
		log.C(ctx).Debugf("Could not generate broker platform credentials for broker (%s): %s", brokerProxyNameAfter, err)
		return
	}

	credentials := &types.BrokerPlatformCredential{
		Username:       username,
		PasswordHash:   passwordHash,
		BrokerID:       brokerAfterUpdate.Resource.ID,
		NotificationID: notification.ID,
	}

	updateRequest := &platform.UpdateServiceBrokerRequest{
		ID:        brokerAfterUpdate.Resource.ID,
		GUID:      existingBroker.GUID,
		Name:      brokerProxyNameAfter,
		BrokerURL: brokerProxyPath,
		Username:  username,
		Password:  password,
	}

	if brokerProxyNameBefore != brokerProxyNameAfter {
		log.C(ctx).Infof("Broker %s was renamed to %s. Triggering broker update...", brokerProxyNameBefore, brokerProxyNameAfter)

		credentialResponse, err := bnh.SMClient.PutCredentials(ctx, credentials)
		if err != nil {
			log.C(ctx).Debugf("Could not update broker platform credentials for broker (%s): %s", brokerAfterUpdate.Resource.Name, err)
			return
		}
		newBroker, err := bnh.BrokerClient.UpdateBroker(ctx, updateRequest)
		if err != nil {
			log.C(ctx).WithError(err).Errorf("Could not update broker name from %s to %s", brokerProxyNameBefore, brokerProxyNameAfter)
			return
		}
		log.C(ctx).Infof("Successfully renamed broker %s to %s", brokerProxyNameBefore, brokerProxyNameAfter)
		bnh.resetBrokerCache(ctx, existingBroker, newBroker)
		bnh.activateCredentials(ctx, credentialResponse)
	} else if bnh.CatalogFetcher != nil {
		log.C(ctx).Infof("Refetching catalog for broker with name %s...", brokerProxyNameAfter)
		credentialResponse, err := bnh.SMClient.PutCredentials(ctx, credentials)
		if err != nil {
			log.C(ctx).Debugf("Could not update broker platform credentials for broker (%s): %s", brokerAfterUpdate.Resource.Name, err)
			return
		}

		if err := bnh.CatalogFetcher.Fetch(ctx, updateRequest); err != nil {
			log.C(ctx).WithError(err).Errorf("error during fetching catalog for platform guid %s and sm id %s", updateRequest.GUID, brokerAfterUpdate.Resource.ID)
			return
		}
		log.C(ctx).Infof("Successfully refetched catalog for platform broker with name %s and URL %s", existingBroker.Name, existingBroker.BrokerURL)
		bnh.resetBrokerCache(ctx, nil, &platform.ServiceBroker{
			GUID:      updateRequest.GUID,
			Name:      updateRequest.Name,
			BrokerURL: updateRequest.BrokerURL,
		})
		bnh.activateCredentials(ctx, credentialResponse)
	} else {
		log.C(ctx).Warn("No catalog fetcher is provided. Cannot update broker catalog in the platform")
	}
}

// OnDelete deletes brokers from the provided notification payload by invoking the proper platform clients
func (bnh *BrokerResourceNotificationsHandler) OnDelete(ctx context.Context, notification *types.Notification) {
	payload := notification.Payload
	log.C(ctx).Debugf("Processing broker delete notification with payload %s...", string(payload))

	brokerPayload, err := bnh.unmarshalPayload(types.DELETED, payload)
	if err != nil {
		log.C(ctx).WithError(err).Error("could not extract broker payload")
		return
	}

	brokerToDelete := brokerPayload.Old
	brokerProxyName := bnh.brokerProxyName(brokerToDelete.Resource)
	brokerProxyPath := bnh.brokerProxyPath(brokerToDelete.Resource)

	if slice.StringsAnyEquals(bnh.BrokerBlacklist, brokerToDelete.Resource.Name) {
		log.C(ctx).Infof("Broker name %s for broker delete notification is part of broker blacklist. Skipping notification...", brokerToDelete.Resource.Name)
		return
	}

	log.C(ctx).Infof("Attempting to find platform broker with name %s in platform...", brokerProxyName)

	existingBroker, err := bnh.BrokerClient.GetBrokerByName(ctx, brokerProxyName)
	if err != nil {
		log.C(ctx).Errorf("Could not find broker with name %s in the platform: %s. No deletion will be attempted", brokerProxyName, err)
		return
	} else if existingBroker == nil {
		log.C(ctx).Errorf("Could not find broker with name %s in the platform. No deletion will be attempted", brokerProxyName)
		return
	}

	if existingBroker.BrokerURL != brokerProxyPath {
		log.C(ctx).Errorf("Platform broker with name %s has an URL %s and is not taken over by SM. No deletion will be attempted", brokerProxyName, existingBroker.BrokerURL)
		return
	}

	log.C(ctx).Infof("Successfully found platform broker with name %s and URL %s. Attempting to delete...", existingBroker.Name, existingBroker.BrokerURL)

	deleteRequest := &platform.DeleteServiceBrokerRequest{
		ID:   brokerToDelete.Resource.ID,
		GUID: existingBroker.GUID,
		Name: brokerProxyName,
	}

	if err := bnh.BrokerClient.DeleteBroker(ctx, deleteRequest); err != nil {
		log.C(ctx).WithError(err).Errorf("error deleting broker with id %s name %s", deleteRequest.GUID, deleteRequest.Name)
		return
	}

	log.C(ctx).Infof("Successfully deleted platform broker with platform ID %s and name %s", existingBroker.GUID, existingBroker.Name)
	bnh.resetBrokerCache(ctx, existingBroker, nil)
}

func (bnh *BrokerResourceNotificationsHandler) unmarshalPayload(operationType types.NotificationOperation, payload json.RawMessage) (brokerPayload, error) {
	result := brokerPayload{}
	if err := json.Unmarshal(payload, &result); err != nil {
		return brokerPayload{}, errors.Wrap(err, "error unmarshaling broker create notification payload")
	}
	if err := result.Validate(operationType); err != nil {
		return brokerPayload{}, errors.Wrap(err, "error validating broker payload")
	}
	return result, nil
}

func (bnh *BrokerResourceNotificationsHandler) brokerProxyPath(broker *types.ServiceBroker) string {
	return bnh.SMPath + "/" + broker.GetID()
}

func (bnh *BrokerResourceNotificationsHandler) brokerProxyName(broker *types.ServiceBroker) string {
	return fmt.Sprintf("%s%s-%s", bnh.ProxyPrefix, broker.Name, broker.ID)
}

func shouldBeTakenOver(brokerFromPlatform *platform.ServiceBroker, brokerFromSM *types.ServiceBroker) bool {
	return brokerFromPlatform.BrokerURL == brokerFromSM.BrokerURL &&
		brokerFromPlatform.Name == brokerFromSM.Name
}

func determineBrokerNameToFind(oldBrokerName, newBrokerName string) string {
	if oldBrokerName != newBrokerName {
		return oldBrokerName
	}
	return newBrokerName
}

func (bnh *BrokerResourceNotificationsHandler) resetBrokerCache(ctx context.Context, oldBroker, newBroker *platform.ServiceBroker) {
	cache, ok := bnh.BrokerClient.(platform.Caching)
	if !ok {
		return
	}

	if oldBroker != nil {
		if err := cache.ResetBroker(ctx, oldBroker, true); err != nil {
			log.C(ctx).Error(err)
		}
	}
	if newBroker != nil {
		if err := cache.ResetBroker(ctx, newBroker, false); err != nil {
			log.C(ctx).Error(err)
		}
	}
}

func (bnh *BrokerResourceNotificationsHandler) activateCredentials(ctx context.Context, credentials *types.BrokerPlatformCredential) {
	log.C(ctx).Infof("Activating credentials with id %s", credentials.ID)
	err := bnh.SMClient.ActivateCredentials(ctx, credentials.ID)
	if err != nil {
		log.C(ctx).WithError(err).Errorf("failed to activate new credentials for broker id %s", credentials.BrokerID)
	}
}
