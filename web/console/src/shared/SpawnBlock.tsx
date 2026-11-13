import { formatMinuteKST } from "./time";
import { spawnStartText, spawnStateLabel, type TaskSpawn } from "./spawn";

/** The spawn request of a task: where it stands, why, when it is looked at
 * next, where it runs and what it waits for. Used on the task page and in the
 * work-status queue. */
export function SpawnBlock({ spawn }: { spawn: TaskSpawn }) {
  const hostJob = [spawn.host, spawn.job].filter(Boolean).join(" · ");
  return (
    <dl className="spawn-block" aria-label="실행 요청">
      <dt>요청 상태</dt>
      <dd>{spawnStateLabel(spawn.state)}</dd>
      {spawn.reason ? (
        <>
          <dt>사유</dt>
          <dd>{spawn.reason}</dd>
        </>
      ) : null}
      {spawn.next_check_at ? (
        <>
          <dt>다음 확인</dt>
          <dd>{formatMinuteKST(spawn.next_check_at) ?? spawn.next_check_at}</dd>
        </>
      ) : null}
      {hostJob ? (
        <>
          <dt>호스트 · 작업</dt>
          <dd>{hostJob}</dd>
        </>
      ) : null}
      {spawn.start_condition ? (
        <>
          <dt>시작 조건</dt>
          <dd>{spawnStartText(spawn.start_condition)}</dd>
        </>
      ) : null}
      <dt>시도</dt>
      <dd>{spawn.attempt}</dd>
    </dl>
  );
}
