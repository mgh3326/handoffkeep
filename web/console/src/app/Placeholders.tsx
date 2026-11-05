// One-line pages for what later steps fill in (hk 3140 C0).

export function Work() {
  return (
    <section className="hk-view">
      <h1>Work</h1>
      <p className="muted">Work is coming with hk 3142.</p>
    </section>
  );
}

/** The /ui/tasks/{id} deep link. It makes no request: the task record arrives
 * with C2 (hk 3142). */
export function TaskPlaceholder({ id }: { id: string }) {
  return (
    <section className="hk-view">
      <h1>Task #{id}</h1>
      <p className="muted">The task record arrives with C2 (hk 3142).</p>
    </section>
  );
}

/** Fleet, grades, docs and any unknown path. */
export function Moved() {
  return (
    <section className="hk-view">
      <p>
        This page has moved. <a href="/ui">Go to Home</a>
      </p>
    </section>
  );
}
