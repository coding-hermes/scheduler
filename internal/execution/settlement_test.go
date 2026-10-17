package execution

import (
	"errors"
	"testing"
)

func completeSettlement() (TerminalOutcome, ConnectorSettlement, IndependentVerification) {
	return TerminalOutcome{OutcomeRef: "outcome-1", State: AttemptStateCompleted, CorrelationID: "corr-1"},
		ConnectorSettlement{SettlementReceiptRef: "connector-settlement-1", QuiescenceReceiptRef: "quiescence-1"},
		IndependentVerification{VerificationRef: "verification-1", VerifierID: "verifier-1", Verdict: VerifierPass}
}

func TestValidateAttemptSettlementRequiresTheTriple(t *testing.T) {
	outcome, connector, independent := completeSettlement()
	ok := AttemptSettlement{ParentRunID: "parent-1", AttemptID: "attempt-1", Outcome: outcome, Connector: connector, Independent: independent}
	if err := ValidateAttemptSettlement(ok); err != nil {
		t.Fatalf("complete settlement refused: %v", err)
	}

	ack := &TransportAck{AckID: "ack-1", MessageID: "msg-1", CorrelationID: "corr-1", ReceivedAt: "2026-10-15T12:00:00Z"}
	ackOnly := ok
	ackOnly.Outcome, ackOnly.Connector, ackOnly.Independent = TerminalOutcome{}, ConnectorSettlement{}, IndependentVerification{}
	ackOnly.TransportAck = ack
	if err := ValidateAttemptSettlement(ackOnly); !errors.Is(err, ErrTransportAckIsNotCompletion) {
		t.Fatalf("ack-only settlement error=%v, want %v", err, ErrTransportAckIsNotCompletion)
	}

	noSettlement := ok
	noSettlement.Connector = ConnectorSettlement{}
	if err := ValidateAttemptSettlement(noSettlement); !errors.Is(err, ErrSettlementIncomplete) {
		t.Fatalf("missing connector settlement error=%v", err)
	}

	noVerification := ok
	noVerification.Independent = IndependentVerification{}
	if err := ValidateAttemptSettlement(noVerification); !errors.Is(err, ErrSettlementIncomplete) {
		t.Fatalf("missing independent verification error=%v", err)
	}

	noOutcome := ok
	noOutcome.Outcome = TerminalOutcome{}
	if err := ValidateAttemptSettlement(noOutcome); !errors.Is(err, ErrSettlementIncomplete) {
		t.Fatalf("missing terminal outcome error=%v", err)
	}

	nonTerminal := ok
	nonTerminal.Outcome.State = AttemptStateWorking
	if err := ValidateAttemptSettlement(nonTerminal); !errors.Is(err, ErrSettlementIncomplete) {
		t.Fatalf("non-terminal outcome error=%v", err)
	}

	badVerdict := ok
	badVerdict.Independent.Verdict = "maybe"
	if err := ValidateAttemptSettlement(badVerdict); !errors.Is(err, ErrSettlementIncomplete) {
		t.Fatalf("open verdict error=%v", err)
	}
}

func TestValidateParentSettlementDescendantAndQuiescenceRules(t *testing.T) {
	outcome, connector, independent := completeSettlement()
	base := ParentSettlement{
		ParentRunID: "parent-1",
		Outcome:     outcome,
		Connector:   connector,
		Independent: independent,
	}
	if err := ValidateParentSettlement(base); err != nil {
		t.Fatalf("empty descendant set refused: %v", err)
	}

	activeDescendant := base
	activeDescendant.Descendants = []AttemptQuiescence{{AttemptID: "attempt-1", Terminal: false}}
	if err := ValidateParentSettlement(activeDescendant); !errors.Is(err, ErrUnsettledDescendants) {
		t.Fatalf("active descendant error=%v", err)
	}

	unsettledConnector := base
	unsettledConnector.Descendants = []AttemptQuiescence{{AttemptID: "attempt-1", Terminal: true}}
	if err := ValidateParentSettlement(unsettledConnector); !errors.Is(err, ErrUnsettledDescendants) {
		t.Fatalf("unsettled connector error=%v", err)
	}

	reconciled := base
	reconciled.Descendants = []AttemptQuiescence{
		{AttemptID: "attempt-1", Terminal: true, ConnectorSettled: true, TransportSettled: true},
		{AttemptID: "attempt-2", Terminal: true, ReconciledNoStart: true, TransportSettled: true},
	}
	if err := ValidateParentSettlement(reconciled); err != nil {
		t.Fatalf("quiescent descendants refused: %v", err)
	}

	unsettledTransport := base
	unsettledTransport.Descendants = []AttemptQuiescence{{AttemptID: "attempt-1", Terminal: true, ConnectorSettled: true}}
	err := ValidateParentSettlement(unsettledTransport)
	if !errors.Is(err, ErrTransportSettlementOutstanding) {
		t.Fatalf("unsettled dispatch transport error=%v", err)
	}
	if errors.Is(err, ErrUnsettledDescendants) || errors.Is(err, ErrQuiescenceEvidenceRequired) {
		t.Fatalf("transport refusal must be distinct from quiescence errors: %v", err)
	}
	unsettledTransport.Descendants[0].TransportSettled = true
	if err := ValidateParentSettlement(unsettledTransport); err != nil {
		t.Fatalf("settled dispatch transport refused: %v", err)
	}

	cancelling := base
	cancelling.FromParentState = ParentCancelling
	cancelling.Outcome.State = AttemptStateCanceled
	if err := ValidateParentSettlement(cancelling); !errors.Is(err, ErrQuiescenceEvidenceRequired) {
		t.Fatalf("cancellation without quiescence receipt error=%v", err)
	}
	cancelling.QuiescenceReceiptRef = "quiescence-receipt-1"
	if err := ValidateParentSettlement(cancelling); err != nil {
		t.Fatalf("cancellation with quiescence receipt refused: %v", err)
	}

	// Cancellation is monotonic: success cannot follow a cancellation request.
	completedCancel := cancelling
	completedCancel.Outcome.State = AttemptStateCompleted
	if err := ValidateParentSettlement(completedCancel); !errors.Is(err, ErrCancellationMonotonic) {
		t.Fatalf("cancelled parent settled completed: error=%v", err)
	}

	// Process-backed descendants additionally need zero-descendant quiescence.
	processBacked := base
	processBacked.Descendants = []AttemptQuiescence{{AttemptID: "attempt-1", Terminal: true, ConnectorSettled: true, ProcessBacked: true, TransportSettled: true}}
	if err := ValidateParentSettlement(processBacked); !errors.Is(err, ErrQuiescenceEvidenceRequired) {
		t.Fatalf("process-backed descendant without quiescence error=%v", err)
	}
	processBacked.Descendants[0].QuiescenceRef = "quiescence-1"
	if err := ValidateParentSettlement(processBacked); !errors.Is(err, ErrQuiescenceEvidenceRequired) {
		t.Fatalf("process-backed parent without zero-descendant authority error=%v", err)
	}
	processBacked.QuiescenceReceiptRef = "quiescence-receipt-1"
	if err := ValidateParentSettlement(processBacked); err != nil {
		t.Fatalf("process-backed quiescence refused: %v", err)
	}

	ackOnly := base
	ackOnly.Outcome, ackOnly.Connector, ackOnly.Independent = TerminalOutcome{}, ConnectorSettlement{}, IndependentVerification{}
	ackOnly.TransportAck = &TransportAck{AckID: "ack-1"}
	if err := ValidateParentSettlement(ackOnly); !errors.Is(err, ErrTransportAckIsNotCompletion) {
		t.Fatalf("ack as parent completion error=%v", err)
	}
}

func TestValidateForemanReport(t *testing.T) {
	outcome, connector, independent := completeSettlement()
	report := ForemanReport{
		ParentRunID:  "parent-1",
		InvocationID: "invocation-1",
		Outcome:      outcome,
		Connector:    connector,
		Independent:  independent,
	}
	if err := ValidateForemanReport(report); err != nil {
		t.Fatalf("report refused: %v", err)
	}
	report.Connector = ConnectorSettlement{}
	if err := ValidateForemanReport(report); !errors.Is(err, ErrSettlementIncomplete) {
		t.Fatalf("report without connector settlement error=%v", err)
	}
}
