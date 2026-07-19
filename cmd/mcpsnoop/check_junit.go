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
		for _, signal := range checkSignalOrder {
			count := summary.count(signal)
			testcase := checkJUnitCase{
				Classname: "mcpsnoop.check",
				Name:      fmt.Sprintf("%s/%s", summary.sessionID, signal),
				Time:      "0",
			}
			if selected[signal] && count > 0 {
				reason := checkSignalFailureReason(summary.sessionID, signal, count)
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
	case checkDrift:
		singular, plural = "tool definition change", "tool definition changes"
	default:
		singular, plural = "signal", "signals"
	}
	word := plural
	if count == 1 {
		word = singular
	}
	return fmt.Sprintf("session %s has %d %s", sessionID, count, word)
}
