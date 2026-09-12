package core

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"sync"
)

// ActionPrincipal is the transport-authenticated identity bound to a hosted
// action. Implementations must not accept these fields from the agent command.
type ActionPrincipal struct {
	Platform   string `json:"platform"`
	UserID     string `json:"user_id"`
	ChatID     string `json:"chat_id"`
	SessionKey string `json:"session_key"`
	Project    string `json:"project"`
	MessageID  string `json:"message_id"`
}

// ActionRef identifies an already-staged action. It contains no business data;
// the host implementation reloads the canonical preview from its own ledger.
type ActionRef struct {
	Kind     string `json:"kind"`
	ChangeID string `json:"change_id"`
}

type ActionDecision string

const (
	ActionApprove ActionDecision = "approve"
	ActionModify  ActionDecision = "modify"
	ActionCancel  ActionDecision = "cancel"
)

// ActionHostResult is presentation-ready output from the action adapter. The
// adapter owns the business wording; core only routes and refreshes the card.
type ActionHostResult struct {
	Kind       string
	Status     string
	Code       string
	ApprovalID string
	ChangeID   string
	Card       *Card
	Replayed   bool
	// Next is one separately approved action, never an instruction to execute it.
	Next *ActionHostResult
}

// ActionHost is the narrow seam between an agent turn and a host-owned action.
// Persistence, approval CAS, business writes, and receipts live behind it.
type ActionHost interface {
	Kind() string
	Match(Event) (ActionRef, bool)
	SessionEnv(string) ([]string, error)
	Begin(context.Context, ActionRef, ActionPrincipal, string, Language) (ActionHostResult, error)
	BindCard(context.Context, string, ActionPrincipal) error
	Claim(context.Context, string, ActionDecision, ActionPrincipal, Language) (ActionHostResult, bool, error)
	Execute(context.Context, string, ActionPrincipal, Language) (ActionHostResult, error)
}

// TrustedCardAction is populated by the platform callback, never by a model
// message. SessionKey routes the card; UserID and ChatID come from the signed
// platform callback and are used for authorization.
type TrustedCardAction struct {
	Kind       string
	ApprovalID string
	Decision   ActionDecision
	Principal  ActionPrincipal
	Language   Language
}

type TrustedCardActionResponse struct {
	Card      *Card
	Toast     string
	ToastType string
	Complete  func() TrustedCardActionResponse
}

type TrustedCardActionHandler func(TrustedCardAction) TrustedCardActionResponse

// hostedSharedCard copies the envelope without mutating adapter-owned cards.
func hostedSharedCard(card *Card) *Card {
	if card == nil {
		return nil
	}
	shared := *card
	shared.SharedUpdate = true
	return &shared
}

// TrustedCardActionNavigable is intentionally separate from command-card
// navigation. It preserves the platform-authenticated callback identity.
type TrustedCardActionNavigable interface {
	SetTrustedCardActionHandler(TrustedCardActionHandler)
}

func newActionToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (e *Engine) prepareHostedAction(state *interactiveState, ref ActionRef) (ActionHost, ActionHostResult, ActionPrincipal, bool) {
	e.actionMu.RLock()
	host := e.actionHost
	e.actionMu.RUnlock()
	host = actionHostForKind(host, ref.Kind)
	if host == nil {
		return nil, ActionHostResult{}, ActionPrincipal{}, false
	}

	state.mu.Lock()
	principal := state.currentPrincipal
	token := state.actionToken
	state.mu.Unlock()

	result, err := host.Begin(e.ctx, ref, principal, token, e.i18n.CurrentLang())
	if err != nil {
		slog.Error("action host begin failed", "action_kind", ref.Kind, "error", err)
		result = ActionHostResult{
			Kind:   ref.Kind,
			Status: "failed",
			Card: NewCard().
				Title(e.i18n.T(MsgHostedActionBeginFailedTitle), "red").
				Markdown(e.i18n.T(MsgHostedActionBeginFailedBody)).
				Build(),
		}
	}
	return host, result, principal, true
}

func (e *Engine) publishHostedAction(host ActionHost, result ActionHostResult, principal ActionPrincipal, p Platform, replyCtx any) error {
	if result.Status != "pending" {
		if result.Card != nil {
			e.replyWithCard(p, replyCtx, result.Card)
		}
		return nil
	}
	err := e.publishHostedActionContext(e.ctx, host, result, principal, p, replyCtx)
	if err != nil {
		// Only the legacy handoff needs an out-of-band failure notification.
		// The model tool path returns unavailable in its normal tool result.
		e.replyWithCard(p, replyCtx, e.hostedActionFailureCard())
	}
	return err
}

func (e *Engine) publishHostedActionContext(ctx context.Context, host ActionHost, result ActionHostResult, principal ActionPrincipal, p Platform, replyCtx any) error {
	if result.Status != "pending" {
		return nil
	}
	if result.ApprovalID == "" || result.Card == nil {
		slog.Error("action host returned incomplete pending approval", "action_kind", result.Kind)
		return errors.New("incomplete hosted approval")
	}

	publisher, canPublish := p.(HostedActionCardPublisher)
	refresher, canRefresh := p.(CardMessageRefresher)
	if !canPublish || !canRefresh {
		slog.Error("platform cannot bind an exact hosted approval card", "platform", p.Name(), "action_kind", result.Kind)
		return errors.New("hosted approval cards unsupported")
	}
	if validator, ok := p.(CardValidator); ok {
		if err := validator.ValidateCard(hostedSharedCard(result.Card), principal.SessionKey); err != nil {
			return err
		}
	} else if result.Card.MaxBytes > 0 {
		return errors.New("platform cannot validate complete hosted approval size")
	}

	placeholder := NewCard().
		Title(e.i18n.T(MsgHostedActionPreparingTitle), "blue").
		Markdown(e.i18n.T(MsgHostedActionPreparingBody)).
		Build()
	messageID, err := publisher.ReplyHostedActionPlaceholder(ctx, replyCtx, hostedSharedCard(placeholder))
	if err != nil || messageID == "" {
		slog.Error("hosted approval placeholder publish failed", "platform", p.Name(), "action_kind", result.Kind)
		return errors.New("hosted approval publish failed")
	}

	principal.MessageID = messageID
	if err := host.BindCard(ctx, result.ApprovalID, principal); err != nil {
		slog.Error("hosted approval card binding failed", "platform", p.Name(), "action_kind", result.Kind)
		if refreshErr := refresher.RefreshCardMessage(ctx, messageID, principal.SessionKey, e.hostedActionFailureCard()); refreshErr != nil {
			slog.Warn("hosted approval binding failure card refresh failed", "platform", p.Name())
		}
		return errors.New("hosted approval binding failed")
	}
	if err := refresher.RefreshCardMessage(ctx, messageID, principal.SessionKey, hostedSharedCard(result.Card)); err != nil {
		slog.Error("hosted approval activation failed", "platform", p.Name(), "action_kind", result.Kind)
		if refreshErr := refresher.RefreshCardMessage(ctx, messageID, principal.SessionKey, e.hostedActionFailureCard()); refreshErr != nil {
			slog.Warn("hosted approval activation failure card refresh failed", "platform", p.Name())
		}
		return errors.New("hosted approval activation failed")
	}
	return nil
}

func (e *Engine) hostedActionFailureCard() *Card {
	return hostedSharedCard(NewCard().
		Title(e.i18n.T(MsgHostedActionFailedTitle), "red").
		Markdown(e.i18n.T(MsgHostedActionFailedBody)).
		Build())
}

func (e *Engine) failClosedHostedTurn(sessionKey string, state *interactiveState, err error) {
	slog.Error("hosted action could not terminate the agent permission request", "session", sessionKey, "error", err)
	state.mu.Lock()
	state.eventsNeedResync = true
	state.mu.Unlock()
	state.markStopped()
	go e.cleanupInteractiveState(sessionKey, state)
}

func (e *Engine) handleTrustedCardAction(p Platform, action TrustedCardAction) TrustedCardActionResponse {
	e.actionMu.RLock()
	host := e.actionHost
	e.actionMu.RUnlock()
	host = actionHostForKind(host, action.Kind)
	if host == nil || action.ApprovalID == "" {
		return TrustedCardActionResponse{Toast: e.i18n.T(MsgHostedActionInvalidToast), ToastType: "error"}
	}
	if action.Decision != ActionApprove && action.Decision != ActionModify && action.Decision != ActionCancel {
		return TrustedCardActionResponse{Toast: e.i18n.T(MsgHostedActionInvalidDecisionToast), ToastType: "error"}
	}

	// Platform and project are selected by the registered callback, not by card data.
	action.Principal.Platform = p.Name()
	action.Principal.Project = e.name
	if action.Language == LangAuto {
		action.Language = e.i18n.CurrentLang()
	}

	result, execute, err := host.Claim(e.ctx, action.ApprovalID, action.Decision, action.Principal, action.Language)
	if err != nil {
		slog.Error("action host decision claim unconfirmed", "action_kind", action.Kind, "decision", action.Decision)
		return TrustedCardActionResponse{Toast: e.i18n.T(MsgHostedActionResultUnknownToast), ToastType: "error"}
	}
	if result.Status == "failed" && !execute && !result.Replayed {
		// A failed lookup is not proof that this callback owns a state change.
		return TrustedCardActionResponse{Toast: e.i18n.T(MsgHostedActionResultUnknownToast), ToastType: "error"}
	}
	if result.Code == "principal_mismatch" {
		return TrustedCardActionResponse{Toast: e.i18n.T(MsgHostedActionPrincipalMismatchToast), ToastType: "error"}
	}
	if result.Code == "card_mismatch" {
		// A copied/misdirected button must not replace another operation's card.
		return TrustedCardActionResponse{Toast: e.i18n.T(MsgHostedActionInvalidToast), ToastType: "error"}
	}
	if result.Status == "blocked" {
		// A rejected lookup has not acquired a durable transition. A delayed
		// callback must not replace the operation owner's eventual receipt.
		return TrustedCardActionResponse{Toast: e.i18n.T(MsgHostedActionInvalidToast), ToastType: "error"}
	}
	if result.Status == "executing" && !execute && result.Code != "execution_result_unknown" {
		// Another callback already owns execution. Do not return a card here:
		// this response may arrive after the owner has published the final
		// receipt, and replacing it would make the UI move backwards.
		return TrustedCardActionResponse{Toast: e.i18n.T(MsgHostedActionProcessingToast), ToastType: "info"}
	}
	if result.Replayed || result.Code == "execution_result_unknown" {
		// Never let a delayed replay replace the execution owner's newer card.
		toast := e.i18n.T(MsgHostedActionResultUnknownToast)
		if result.Code != "execution_result_unknown" && result.Card != nil && result.Card.Header != nil {
			toast = result.Card.Header.Title
		}
		return TrustedCardActionResponse{Toast: toast, ToastType: "info"}
	}
	response := TrustedCardActionResponse{Card: hostedSharedCard(result.Card)}
	if execute {
		principal := action.Principal
		approvalID := action.ApprovalID
		operationID := result.ChangeID
		if operationID == "" {
			operationID = approvalID
		}
		language := action.Language
		var once sync.Once
		var final TrustedCardActionResponse
		response.Complete = func() TrustedCardActionResponse {
			once.Do(func() {
				completed, completeErr := host.Execute(e.ctx, approvalID, principal, language)
				if completeErr != nil {
					slog.Error("action host execution receipt unconfirmed", "action_kind", action.Kind)
					i18n := NewI18n(language)
					final = TrustedCardActionResponse{Card: hostedSharedCard(NewCard().
						Title(i18n.T(MsgHostedActionUnknownTitle), "orange").
						Markdown(i18n.T(MsgHostedActionUnknownBody)).
						Note(operationID).Build())}
					return
				}
				final.Card = hostedSharedCard(completed.Card)
				if completed.Next != nil && completed.Status == "verified" {
					next := completed.Next
					reconstructor, ok := p.(ReplyContextReconstructor)
					var publishErr error
					if !ok || next.Kind != host.Kind() || next.Status != "pending" || next.Next != nil {
						publishErr = errors.New("invalid or unsupported next approval")
					} else {
						var replyCtx any
						replyCtx, publishErr = reconstructor.ReconstructReplyCtx(principal.SessionKey)
						if publishErr == nil {
							publishErr = e.publishHostedActionContext(e.ctx, host, *next, principal, p, replyCtx)
						}
					}
					if publishErr != nil {
						slog.Warn("next approval delivery could not be confirmed; completed action remains verified", "action_kind", action.Kind)
						// Preserve the parent receipt. A second card delivery failure is
						// not a failure or rollback of the already verified write.
						if final.Card != nil {
							card := *final.Card
							card.Elements = append(append([]CardElement(nil), card.Elements...), CardDivider{}, CardMarkdown{Content: NewI18n(language).T(MsgHostedActionNextFailedBody)})
							final.Card = &card
						}
					}
				}
			})
			return final
		}
	}
	return response
}
