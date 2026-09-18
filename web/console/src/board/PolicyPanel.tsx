import type { PolicyResponse } from "./types";

function PolicyStatus({ status }: { status: PolicyResponse["status"] }) {
  switch (status) {
    case "not_configured":
      return <p className="muted">활성 정책 릴리스가 설정되지 않았습니다.</p>;
    case "invalid_pointer":
      return <p className="muted">policy/active 포인터가 올바른 문서 키를 가리키지 않습니다.</p>;
    case "manifest_missing":
      return <p className="muted">포인터가 가리키는 매니페스트 문서가 없습니다.</p>;
    case "invalid_manifest":
      return <p className="muted">매니페스트 문서 형식이 올바르지 않습니다.</p>;
    default:
      return null;
  }
}

// Policy canon resolves only the exact pointer and manifest keys. Prefix
// listing or search would imply a coverage this panel deliberately does not
// claim.
export function PolicyPanel({ policy }: { policy: PolicyResponse | null }) {
  return (
    <section className="policy-panel">
      <h2>Policy canon</h2>
      {policy === null ? (
        <p className="muted">불러오는 중</p>
      ) : (
        <>
          <p className="policy-meta">
            pointer <a href={policy.pointer_doc_url}>{policy.pointer_key}</a>
            {policy.manifest_key ? (
              <>
                {" "}
                · manifest <a href={policy.manifest_doc_url}>{policy.manifest_key}</a>
              </>
            ) : null}
            {policy.release ? <> · release {policy.release}</> : null}
          </p>
          <PolicyStatus status={policy.status} />
          {policy.status === "ok" ? (
            <>
              {policy.truncated ? <p className="badge board-warning">일부만 표시됨</p> : null}
              <ul className="policy-items">
                {policy.items.map((item) => (
                  <li key={item.key}>
                    <a href={item.doc_url}>{item.title || item.key}</a> <span className="muted">{item.key}</span>
                    {item.exists ? null : <span className="badge">문서 없음</span>}
                  </li>
                ))}
                {policy.items.length === 0 ? <li className="muted">매니페스트 항목이 없습니다.</li> : null}
              </ul>
            </>
          ) : null}
        </>
      )}
    </section>
  );
}
