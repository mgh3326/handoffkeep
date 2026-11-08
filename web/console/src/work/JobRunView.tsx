import type { WorkJob } from "../shared/boardTypes";

const docHref = (key: string) => `/ui/doc/${key.split("/").map(encodeURIComponent).join("/")}`;

/** One job of the 작업 실행 section: 무엇 / 왜 / 다른 후보 / 결과. The text is
 * the server's `run` summary; a job with no recorded selection says so. */
export function JobRunView({ job, pr }: { job: WorkJob; pr?: string }) {
  const { run } = job;
  const skills = job.skills_supported === false ? "skills not supported" : job.skills?.map(s => `${s.name} (${s.calls})`).join(", ") || "skills 없음";
  return <div className="work-job-run">
    <h3>{job.job_id}</h3>
    <dl>
      <dt>무엇</dt><dd>{run.what}</dd>
      <dt>왜</dt><dd>{run.why}{run.override && <p>직접 지정: {run.override}</p>}</dd>
      {run.others.length > 0 && <><dt>다른 후보</dt><dd><ul>{run.others.map((o, i) => <li key={i}>{o}</li>)}</ul></dd></>}
      <dt>결과</dt><dd>{run.result}{job.report_doc && <> · <a href={docHref(job.report_doc)}>보고서</a></>}{pr && <> · <a href={pr}>PR</a></>}</dd>
    </dl>
    <p className="work-job-skills">{skills}</p>
  </div>;
}
