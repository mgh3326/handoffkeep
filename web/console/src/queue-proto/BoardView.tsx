import { VirtualList } from "./VirtualList";
import { CardFields } from "./TaskRow";
import type { Dataset, ProtoState, ProtoTask } from "./types";

type BoardViewProps = {
  dataset: Dataset;
  columns: { state: string; tasks: ProtoTask[] }[];
  density: ProtoState["density"];
  selectedId: number | null;
  onOpen: (id: number, el: HTMLElement) => void;
};

export function BoardView({ dataset, columns, density, selectedId, onOpen }: BoardViewProps) {
  const cardHeight = density === "compact" ? 76 : 96;
  return (
    <div className={`qp-board ${density}`}>
      {columns.map((col) => (
        <section key={col.state} className="qp-col" data-state={col.state}>
          <h3 className="qp-col-head">
            {col.state} <span className="qp-badge" data-col-count={col.state}>{col.tasks.length}</span>
          </h3>
          <VirtualList
            className="qp-col-cards"
            items={col.tasks}
            rowHeight={cardHeight}
            getKey={(task) => task.id}
            renderRow={(task) => (
              <button
                type="button"
                className={`qp-card${task.id === selectedId ? " selected" : ""}`}
                data-task-id={task.id}
                onClick={(event) => onOpen(task.id, event.currentTarget)}
                onKeyDown={(event) => {
                  if (event.key === "Enter") {
                    onOpen(task.id, event.currentTarget);
                  }
                }}
              >
                <CardFields task={task} now={dataset.generatedAt} />
              </button>
            )}
          />
        </section>
      ))}
    </div>
  );
}
