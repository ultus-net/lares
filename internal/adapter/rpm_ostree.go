package adapter

import "encoding/json"

// RPMOstreeDeployment is one deployment in an rpm-ostree status document.
type RPMOstreeDeployment struct {
	Booted   bool   `json:"booted"`
	Staged   bool   `json:"staged"`
	Version  string `json:"version"`
	Checksum string `json:"checksum"`
}

// RPMOstreeStatus is the document `rpm-ostree status --json` reports.
//
// Two readers share this contract: the measure engine names a staged deployment,
// and the reboot gate refuses to reboot while a transaction is held. One type
// keeps a field rename from failing one of them open.
type RPMOstreeStatus struct {
	Deployments []RPMOstreeDeployment `json:"deployments"`
	Transaction json.RawMessage       `json:"transaction"`
}

// ParseRPMOstreeStatus decodes an rpm-ostree status document.
func ParseRPMOstreeStatus(out []byte) (RPMOstreeStatus, error) {
	var status RPMOstreeStatus
	if err := json.Unmarshal(out, &status); err != nil {
		return RPMOstreeStatus{}, err
	}
	return status, nil
}

// Held reports whether the document names a transaction.
//
// The daemon writes `"transaction": null` when idle. A field that is absent also
// means idle, because an older rpm-ostree does not report it.
func (s RPMOstreeStatus) Held() bool {
	return len(s.Transaction) > 0 && string(s.Transaction) != "null"
}
