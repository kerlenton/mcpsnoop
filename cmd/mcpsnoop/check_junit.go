package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

type checkJUnitSuites struct {
	XMLName  xml.Name          `xml:"testsuites"`
	Name     string            `xml:"name,attr"`
	Tests    int               `xml:"tests,attr"`
	Failures int               `xml:"failures,attr"`
	Errors   int               `xml:"errors,attr"`
	Skipped  int               `xml:"skipped,attr"`
	Time     string            `xml:"time,attr"`
	Suites   []checkJUnitSuite `xml:"testsuite"`
}

type checkJUnitSuite struct {
	Name     string           `xml:"name,attr"`
	Tests    int              `xml:"tests,attr"`
	Failures int              `xml:"failures,attr"`
	Errors   int              `xml:"errors,attr"`
	Skipped  int              `xml:"skipped,attr"`
	Time     string           `xml:"time,attr"`
	Cases    []checkJUnitCase `xml:"testcase"`
}

type checkJUnitCase struct {
	Classname string             `xml:"classname,attr"`
	Name      string             `xml:"name,attr"`
	Time      string             `xml:"time,attr"`
	Failure   *checkJUnitFailure `xml:"failure,omitempty"`
	Skipped   *checkJUnitSkipped `xml:"skipped,omitempty"`
}

// checkJUnitSkipped marks a case that ran but verified nothing, which is what a
// first-seen tool baseline is.
type checkJUnitSkipped struct {
	Message string `xml:"message,attr"`
}

type checkJUnitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Body    string `xml:",chardata"`
}

func writeCheckJUnit(w io.Writer, summaries []checkSummary, selected map[checkSignal]bool, assertionFailures [][]string) error {
	payload := buildCheckJUnit(summaries, selected, assertionFailures)
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(payload); err != nil {
		return err
	}
	if err := enc.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

func buildCheckJUnit(summaries []checkSummary, selected map[checkSignal]bool, assertionFailures [][]string) checkJUnitSuites {
	out := checkJUnitSuites{Name: "mcpsnoop check", Time: "0"}
	for i, summary := range summaries {
		suite := checkJUnitSuite{
			Name:  summary.sessionID,
			Tests: len(checkSignalOrder),
			Time:  "0",
			Cases: make([]checkJUnitCase, 0, len(checkSignalOrder)),
		}
		// A first-seen baseline is recorded rather than verified, which the text
		// format says out loud. Without its own case the junit reader sees a fully
		// green suite for a run that checked nothing, which is the state the whole
		// baseline mechanism exists to make visible.
		if summary.baselineCreated {
			suite.Tests++
			suite.Cases = append(suite.Cases, checkJUnitCase{
				Classname: "mcpsnoop.check",
				Name:      summary.sessionID + "/baseline",
				Time:      "0",
				Skipped:   &checkJUnitSkipped{Message: "recorded first-seen tool baseline (trusted, not verified)"},
			})
		}
		for _, signal := range checkSignalOrder {
			count := summary.count(signal)
			testcase := checkJUnitCase{
				Classname: "mcpsnoop.check",
				Name:      fmt.Sprintf("%s/%s", summary.sessionID, signal),
				Time:      "0",
			}
			if selected[signal] && count > 0 {
				reason := checkSignalFailureReason(summary.sessionID, signal, count)
				// A baseline that could not be read is not a tool change, and telling a
				// CI reader to go looking for one sends them after something that never
				// happened. The text format already says which it is.
				if signal == checkDrift && summary.drift.BaselineError != "" {
					reason = "tool baseline error: " + summary.drift.BaselineError
				}
				testcase.Failure = &checkJUnitFailure{
					Message: reason,
					Type:    "mcpsnoop.check." + string(signal),
					Body:    reason,
				}
				suite.Failures++
			}
			suite.Cases = append(suite.Cases, testcase)
		}
		assertions := checkJUnitCase{
			Classname: "mcpsnoop.check",
			Name:      summary.sessionID + "/assertions",
			Time:      "0",
		}
		if i < len(assertionFailures) && len(assertionFailures[i]) > 0 {
			reason := strings.Join(assertionFailures[i], "; ")
			assertions.Failure = &checkJUnitFailure{
				Message: reason,
				Type:    "mcpsnoop.check.assertion",
				Body:    strings.Join(assertionFailures[i], "\n"),
			}
			suite.Failures++
		}
		suite.Cases = append(suite.Cases, assertions)
		suite.Tests++

		out.Tests += suite.Tests
		out.Failures += suite.Failures
		out.Suites = append(out.Suites, suite)
	}
	return out
}

func checkSignalFailureReason(sessionID string, signal checkSignal, count int) string {
	var singular, plural string
	switch signal {
	case checkError:
		singular, plural = "error", "errors"
	case checkInvalid:
		singular, plural = "invalid frame", "invalid frames"
	case checkWarn:
		singular, plural = "warning", "warnings"
	case checkMismatch:
		singular, plural = "routing mismatch", "routing mismatches"
	case checkPending:
		singular, plural = "pending call", "pending calls"
	case checkLate:
		singular, plural = "result after a cancel", "results after a cancel"
	case checkDrift:
		singular, plural = "tool definition change", "tool definition changes"
	case checkDeprecated:
		singular, plural = "deprecated protocol feature", "deprecated protocol features"
	case checkIncomplete:
		singular, plural = "dropped frame", "dropped frames"
	default:
		singular, plural = "signal", "signals"
	}
	word := plural
	if count == 1 {
		word = singular
	}
	return fmt.Sprintf("session %s has %d %s", sessionID, count, word)
}
