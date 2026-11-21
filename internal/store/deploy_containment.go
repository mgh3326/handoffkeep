package store

// The containment rule the deploy readers share (hk 3036; design
// hk:doc design/2026-11-06/hk-work-record-design-v1.2 §13.1 R6). SettleMerged
// reads it over the services, deploys and deploy_changes tables. The attempt
// writers that filled those tables (StartDeploy, FinishDeploy) and their
// /v1/deploys and /v1/services routes were removed in hk 5913: no live caller
// used them, and deploys reach hk as deploy-record documents (deploy_record.go).
// The tables keep the rows written before that; dropping them is a later
// contract step.

// containedSQL is the one containment rule (§13.1 R6): change change is
// contained on (service, target) when the newest successful attempt of the
// service covering the target that carries the change as included is a
// deploy, not a rollback. The arguments are SQL expressions.
func containedSQL(service, target, change string) string {
	return `COALESCE((SELECT d.action='deploy' FROM deploys d JOIN deploy_changes dc ON dc.deploy_id=d.id
	WHERE d.service=` + service + ` AND d.state='success' AND ` + target + `=ANY(d.targets) AND dc.change_id=` + change + ` AND dc.included
	ORDER BY d.finished_at DESC, d.id DESC LIMIT 1), false)`
}

// changeDeployedSQL says whether change c needs nothing more to be deployed:
// it is contained on every current target of every service that ever
// included it. A change no service included needs no deploy when every
// service of its repo is deploy_mode none or left it out by the path filter
// (a repo without a service has no deploy flow: merged is the end, §2).
func changeDeployedSQL() string {
	includedBy := `SELECT d.service FROM deploy_changes dc JOIN deploys d ON d.id=dc.deploy_id WHERE dc.change_id=c.id AND dc.included`
	return `CASE WHEN EXISTS(` + includedBy + `)
	THEN NOT EXISTS(SELECT 1 FROM services s, unnest(s.targets) AS t(target) WHERE s.name IN (` + includedBy + `) AND NOT ` + containedSQL("s.name", "t.target", "c.id") + `)
	ELSE NOT EXISTS(SELECT 1 FROM services s WHERE s.repo=c.repo AND s.deploy_mode<>'none'
		AND NOT EXISTS(SELECT 1 FROM deploy_changes dc JOIN deploys d ON d.id=dc.deploy_id WHERE d.service=s.name AND dc.change_id=c.id AND NOT dc.included))
	END`
}
