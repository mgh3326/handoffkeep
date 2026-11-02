import { getJSON } from "../../shared/http";
import { usePoll } from "../../shared/usePoll";
type AttentionData = {
  tasks: { decisions_pending: number; dispositions_open: number };
};
async function load() {
  const data = await getJSON<AttentionData>("/ui/api/glance");
  if (
    !data?.tasks ||
    typeof data.tasks.decisions_pending !== "number" ||
    typeof data.tasks.dispositions_open !== "number"
  )
    throw new Error("Malformed attention counts");
  return data;
}
export function Attention() {
  const { data, failed } = usePoll(load, 30000);
  return (
    <section className="hd-card">
      <h2>Needs your attention</h2>
      <p>
        This queue shows actionable work, without unread or notification state.
      </p>
      {failed ? (
        <p role="alert">Attention refresh failed · last good data retained</p>
      ) : null}
      <p>
        <a href="/ui/decisions">
          {data
            ? `${data.tasks.decisions_pending} decisions · ${data.tasks.dispositions_open} dispositions`
            : "Decision counts unavailable"}
        </a>
      </p>
      <p>
        <a href="/ui/queue?state=verifying">Work awaiting review →</a>
      </p>
    </section>
  );
}
