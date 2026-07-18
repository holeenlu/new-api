package service

import "github.com/gin-gonic/gin"

const subscriptionOAuthRequestStateKey = "subscription_oauth_request_state"

type subscriptionOAuthRequestState struct {
	attempt           *SubscriptionOAuthAttemptTarget
	retryDisabled     bool
	credentialPreview string
	credentialPath    []string
	effectiveGroup    string
}

func subscriptionOAuthState(c *gin.Context, create bool) *subscriptionOAuthRequestState {
	if c == nil {
		return nil
	}
	if value, exists := c.Get(subscriptionOAuthRequestStateKey); exists {
		if state, ok := value.(*subscriptionOAuthRequestState); ok {
			return state
		}
	}
	if !create {
		return nil
	}
	state := &subscriptionOAuthRequestState{}
	c.Set(subscriptionOAuthRequestStateKey, state)
	return state
}

func SetSubscriptionOAuthEffectiveGroup(c *gin.Context, group string) {
	if state := subscriptionOAuthState(c, true); state != nil {
		state.effectiveGroup = group
	}
}

func SubscriptionOAuthEffectiveGroup(c *gin.Context) string {
	if state := subscriptionOAuthState(c, false); state != nil {
		return state.effectiveGroup
	}
	return ""
}

func RecordSubscriptionOAuthCredential(c *gin.Context, fingerprint string) {
	if state := subscriptionOAuthState(c, true); state != nil {
		preview := fingerprint
		if len(preview) > 12 {
			preview = preview[:12]
		}
		state.credentialPreview = preview
		state.credentialPath = append(state.credentialPath, preview)
	}
}

func SubscriptionOAuthCredentialPreview(c *gin.Context) string {
	if state := subscriptionOAuthState(c, false); state != nil {
		return state.credentialPreview
	}
	return ""
}

func SubscriptionOAuthCredentialPath(c *gin.Context) []string {
	if state := subscriptionOAuthState(c, false); state != nil {
		return append([]string(nil), state.credentialPath...)
	}
	return nil
}
