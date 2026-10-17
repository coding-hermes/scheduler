package execution

import "fmt"

// TransportAck is a transport-layer acknowledgement (Crier/A2A). It completes
// delivery only: it can never settle an attempt or a parent, and it never
// carries a completion verdict. The type exists so an ack cannot be
// presented where settlement evidence is required.
type TransportAck struct {
	AckID         string `json:"ack_id"`
	MessageID     string `json:"message_id"`
	CorrelationID string `json:"correlation_id"`
	ReceivedAt    string `json:"received_at"`
}

// TerminalOutcome is the durable tool → foreman terminal outcome: the
// machine-readable result reference and terminal state. Requested and actual
// served identity stay separate evidence elsewhere; this record only points at
// the durable outcome.
type TerminalOutcome struct {
	OutcomeRef    string `json:"outcome_ref"`
	State         string `json:"state"`
	FailureClass  string `json:"failure_class"`
	CorrelationID string `json:"correlation_id"`
}

// TerminalOutcomeStates is the closed terminal vocabulary of a durable
// outcome. It maps onto the landed attempt/run terminal states.
var TerminalOutcomeStates = map[string]bool{
	AttemptStateCompleted: true,
	AttemptStateFailed:    true,
	AttemptStateCanceled:  true,
}

// ConnectorSettlement is the connector/transport settlement receipt pair:
// settlement plus, when processes were involved, zero-descendant quiescence.
type ConnectorSettlement struct {
	SettlementReceiptRef string `json:"settlement_receipt_ref"`
	QuiescenceReceiptRef string `json:"quiescence_receipt_ref"`
}

// IndependentVerification is the independent verifier's result reference —
// never the foreman's own word for its work. Verdict is closed to pass/fail.
type IndependentVerification struct {
	VerificationRef string `json:"verification_ref"`
	VerifierID      string `json:"verifier_id"`
	Verdict         string `json:"verdict"`
}

// Closed independent-verifier verdicts.
const (
	VerifierPass = "pass"
	VerifierFail = "fail"
)

// AttemptSettlement is the complete settlement evidence for one attempt. The
// optional TransportAck is retained as transport evidence only.
type AttemptSettlement struct {
	ParentRunID  string                  `json:"parent_run_id"`
	AttemptID    string                  `json:"attempt_id"`
	Outcome      TerminalOutcome         `json:"outcome"`
	Connector    ConnectorSettlement     `json:"connector"`
	Independent  IndependentVerification `json:"independent"`
	TransportAck *TransportAck           `json:"transport_ack,omitempty"`
}

// AttemptQuiescence is one descendant's quiescence fact as observed by the
// store at parent settlement time.
type AttemptQuiescence struct {
	AttemptID         string
	Terminal          bool
	ConnectorSettled  bool
	ReconciledNoStart bool
}

// ParentSettlement is the complete settlement evidence for the parent run.
type ParentSettlement struct {
	ParentRunID          string                  `json:"parent_run_id"`
	Outcome              TerminalOutcome         `json:"outcome"`
	Connector            ConnectorSettlement     `json:"connector"`
	Independent          IndependentVerification `json:"independent"`
	FromParentState      ParentState             `json:"from_parent_state"`
	QuiescenceReceiptRef string                  `json:"quiescence_receipt_ref"`
	Descendants          []AttemptQuiescence     `json:"descendants"`
	TransportAck         *TransportAck           `json:"transport_ack,omitempty"`
}

// ValidateAttemptSettlement fail-closed validates one attempt settlement. A
// transport ack alone is never completion: when only an ack is present the
// refusal is explicit.
func ValidateAttemptSettlement(s AttemptSettlement) error {
	if s.ParentRunID == "" || s.AttemptID == "" {
		return fmt.Errorf("%w: missing parent/attempt identity", ErrSettlementIncomplete)
	}
	return validateSettlementTriple(s.Outcome, s.Connector, s.Independent, s.TransportAck)
}

// ValidateParentSettlement fail-closed validates parent settlement evidence
// and the descendant quiescence facts. Cancellation additionally demands a
// quiescence receipt: the parent settles only after every descendant has
// settled or been proven never started.
func ValidateParentSettlement(s ParentSettlement) error {
	if s.ParentRunID == "" {
		return fmt.Errorf("%w: missing parent identity", ErrSettlementIncomplete)
	}
	if err := validateSettlementTriple(s.Outcome, s.Connector, s.Independent, s.TransportAck); err != nil {
		return err
	}
	if s.FromParentState == ParentCancelling && s.QuiescenceReceiptRef == "" {
		return ErrQuiescenceEvidenceRequired
	}
	for _, d := range s.Descendants {
		if !d.Terminal {
			return fmt.Errorf("%w: attempt %s still active", ErrUnsettledDescendants, d.AttemptID)
		}
		if !d.ConnectorSettled && !d.ReconciledNoStart {
			return fmt.Errorf("%w: attempt %s has no connector settlement", ErrUnsettledDescendants, d.AttemptID)
		}
	}
	return nil
}

// validateSettlementTriple is the shared settlement authority rule: a durable
// terminal outcome, a connector settlement and an independent verification
// reference are all required, and a transport ack alone is never completion.
func validateSettlementTriple(outcome TerminalOutcome, connector ConnectorSettlement, independent IndependentVerification, ack *TransportAck) error {
	if outcome.OutcomeRef == "" && connector.SettlementReceiptRef == "" && independent.VerificationRef == "" && ack != nil {
		return ErrTransportAckIsNotCompletion
	}
	if outcome.OutcomeRef == "" || !TerminalOutcomeStates[outcome.State] {
		return fmt.Errorf("%w: durable terminal outcome missing or non-terminal", ErrSettlementIncomplete)
	}
	if connector.SettlementReceiptRef == "" {
		return fmt.Errorf("%w: connector settlement missing", ErrSettlementIncomplete)
	}
	if independent.VerificationRef == "" || (independent.Verdict != VerifierPass && independent.Verdict != VerifierFail) {
		return fmt.Errorf("%w: independent verification reference missing", ErrSettlementIncomplete)
	}
	return nil
}
