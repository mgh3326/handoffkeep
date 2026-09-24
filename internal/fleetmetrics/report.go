package fleetmetrics

import "time"

// Coverage says how many units of a population carried the identifiers a
// metric needs. Missing is Total-Linked by construction; Detail breaks the
// linked count down by link source (structured field vs text-derived) or
// names why the rest are missing.
type Coverage struct {
	What   string         `json:"what"`
	Linked int            `json:"linked"`
	Total  int            `json:"total"`
	Detail map[string]int `json:"detail,omitempty"`
}

func (c Coverage) Missing() int { return c.Total - c.Linked }

// Dist summarizes a set of observed values. It is nil in the report when no
// value was observed — never a zero-valued summary.
type Dist struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50"`
	P90 float64 `json:"p90"`
	Max float64 `json:"max"`
}

type Report struct {
	Since       time.Time `json:"since"`
	Until       time.Time `json:"until"`
	CollectedAt time.Time `json:"collected_at"`
	LeadTime    LeadTime  `json:"lead_time"`
	Rounds      Rounds    `json:"rounds"`
	EmptySlots  Slots     `json:"empty_slots"`
	Decisions   Decisions `json:"decisions"`
	NormalPath  Normal    `json:"normal_path"`
	Sources     []string  `json:"sources"`
	Notes       []string  `json:"notes,omitempty"`
}

// LeadTime is metric 1: first formal order → usable result, in hours.
type LeadTime struct {
	// Deploy is the group whose repo has a deploy-record stream: usable =
	// the first successful deploy record that includes the merge.
	Deploy *Dist `json:"deploy"`
	// MergeOnly is the group whose repo has no deploy stream: usable = merge.
	MergeOnly *Dist `json:"merge_only"`
	// Phases decomposes order→usable for tasks with every boundary observed.
	Phases        map[string]*Dist `json:"phases"`
	Completed     int              `json:"completed"`
	OpenAge       *Dist            `json:"open_age"`
	OpenCount     int              `json:"open_count"`
	OpenUnordered int              `json:"open_unordered"`
	Coverage      []Coverage       `json:"coverage"`
}

// Rounds is metric 2: verify rounds per contract lineage.
type Rounds struct {
	PerLineage   *Dist          `json:"per_lineage"`
	OverThree    int            `json:"over_three"`
	Lineages     int            `json:"lineages"`
	FixRounds    int            `json:"fix_rounds"`
	Cause        map[string]int `json:"cause"`
	RepsRounds   *Dist          `json:"reps_rounds"`
	RepsAgree    int            `json:"reps_agree"`
	RepsDisagree int            `json:"reps_disagree"`
	Coverage     []Coverage     `json:"coverage"`
}

// Slots is metric 3: slot-minutes empty while ready work existed.
type Slots struct {
	// SlotMinutes is the denominator: capacity × minutes on covered machines.
	SlotMinutes int `json:"slot_minutes"`
	// Working + HeldIdle + Empty + Unobserved = SlotMinutes (Overflow is the
	// occupancy beyond capacity, not part of the partition).
	Working    int `json:"working"`
	HeldIdle   int `json:"held_idle"`
	Empty      int `json:"empty"`
	Unobserved int `json:"unobserved"`
	Overflow   int `json:"overflow"`
	// EmptyWithReady is the numerator's lower bound (every unobserved
	// unit assumed to hold its slot); EmptyWithReadyMax the upper bound
	// (every unobserved unit assumed gone). Equal when nothing is unobserved.
	EmptyWithReady    int            `json:"empty_with_ready"`
	EmptyWithReadyMax int            `json:"empty_with_ready_max"`
	ByReason          map[string]int `json:"by_reason"`
	PerMachine        []MachineSlots `json:"per_machine"`
	Coverage          []Coverage     `json:"coverage"`
}

type MachineSlots struct {
	Machine           string `json:"machine"`
	Slots             int    `json:"slots"`
	SlotMinutes       int    `json:"slot_minutes"`
	Working           int    `json:"working"`
	HeldIdle          int    `json:"held_idle"`
	Empty             int    `json:"empty"`
	Unobserved        int    `json:"unobserved"`
	EmptyWithReady    int    `json:"empty_with_ready"`
	EmptyWithReadyMax int    `json:"empty_with_ready_max"`
}

// Decisions is metric 4: decision-request dwell, in hours.
type Decisions struct {
	Requests        int              `json:"requests"`
	ToAnswer        *Dist            `json:"to_answer"`
	AnswerToDeliver *Dist            `json:"answer_to_deliver"`
	AnswerToConsume *Dist            `json:"answer_to_consume"`
	ByKind          map[string]int   `json:"by_kind"`
	AnsweredBy      map[string]int   `json:"answered_by"`
	Open            int              `json:"open"`
	OldestOpen      *float64         `json:"oldest_open_hours,omitempty"`
	OldestOpenRef   string           `json:"oldest_open_ref,omitempty"`
	PerKind         map[string]*Dist `json:"per_kind_to_answer"`
	Coverage        []Coverage       `json:"coverage"`
}

// Normal is metric 5: terminal tasks finished without operational repair.
type Normal struct {
	Terminal     int            `json:"terminal"`
	Classified   int            `json:"classified"`
	NormalPath   int            `json:"normal_path"`
	Repaired     int            `json:"repaired"`
	Dropped      int            `json:"dropped"`
	ByRepair     map[string]int `json:"by_repair"`
	OpenRepaired int            `json:"open_repaired"`
	OpenOldest   *float64       `json:"open_repaired_oldest_hours,omitempty"`
	Coverage     []Coverage     `json:"coverage"`
}
