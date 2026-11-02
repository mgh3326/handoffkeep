import type { Density } from "./types";

export function DensityChoice({
  density,
  onChange,
}: {
  density: Density;
  onChange: (d: Density) => void;
}) {
  return (
    <div role="group" aria-label="행 밀도">
      {(["compact", "comfortable"] satisfies Density[]).map((d) => (
        <button
          className="hk-btn"
          key={d}
          aria-pressed={density === d}
          onClick={() => onChange(d)}
        >
          {d === "compact" ? "행 40px · 1줄" : "행 56px · 2줄"}
        </button>
      ))}
    </div>
  );
}
