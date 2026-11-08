"""Build queries.jsonl (30 known-item queries) from the exported corpus.

Each query has a gold set of record keys: "task:<id>" or "doc:<id>". Gold is
either curated (the record the query was written from) or lexical (every
record whose title/body contains the stated string). Run from bench/searchbench.
"""
import json, sys

def load(name):
    return [json.loads(line) for line in open(f"corpus/{name}", encoding="utf-8")]

T = load("tasks.jsonl")
D = load("documents.jsonl")
C = load("comments.jsonl")

def task_titles_with(s):
    return [f"task:{t['id']}" for t in T if s in t["title"]]

def docs_with(s):
    return [f"doc:{d['id']}" for d in D if s in d["body"]]

def docs_with_all(*parts):
    return [f"doc:{d['id']}" for d in D if all(p in d["body"] for p in parts)]

def comment_task_ids(s):
    return sorted({c["task_id"] for c in C if s in c["body"]})

Q = []
def add(text, category, gold, basis):
    Q.append({"id": f"q{len(Q)+1:02d}", "text": text, "category": category, "gold": gold, "gold_basis": basis})

# Curated known-item queries: the gold record is the one the query was written from.
add("태스크 작성 규칙", "spacing-baseline", ["task:629"], "curated: title of 629")
add("태스크작성규칙", "spacing-compound", ["task:629"], "curated: 629, spaces removed")
add("콘솔 결정 답변 경로", "korean-phrase", ["task:48"], "curated: title of 48")
add("결정 답변 경로를", "particle", ["task:48"], "curated: 48 with particle 를")
add("panewire 허브 chat", "english-term-in-korean", ["task:343"], "curated: 343 title says /chat")
add("워커 정지 감지", "korean-phrase", ["task:355"], "curated: title of 355")
add("워커 정지를 감지하는", "particle-inflection", ["task:355"], "curated: 355 with particle/inflection")
add("읽기전용 클라이언트", "korean-compound", ["task:11"], "curated: title of 11")
add("적대 검증", "spacing-variant", ["task:11"], "curated: 11 has 적대검증")
add("parked-car 자산 리뷰", "english-term-in-korean", ["task:1313"], "curated: title of 1313")
add("스폰 선택 시뮬레이션", "korean-for-english-title", ["task:2409", "task:2411", "task:2412"], "curated: the three 'Spawn selection simulation' tasks")
add("rt-parity probe grade A+ T1", "english-only", ["task:2265"], "curated: title of 2265")
add("drawer 개요 펼치기", "korean-phrase", ["task:628"], "curated: title of 628")
add("body_doc 연결 보정", "english-term-in-korean", ["task:630"], "curated: title of 630")
add("append-only 보정", "english-term-in-korean", ["task:630"], "curated: 630 title")
add("AT 재시작 계획", "2-3-char-acronym", ["task:2512"], "curated: title of 2512")
add("decision_table_validate 검증", "identifier-plus-korean", ["task:29"], "curated: title of 29")
add("배포를", "particle-compound", task_titles_with("배포를"), "lexical: task titles containing 배포를")
add("배포가", "particle", task_titles_with("배포가"), "lexical: task titles containing 배포가")
add("배포검증", "compound", docs_with("배포검증"), "lexical: docs whose body contains 배포검증")
add("배포 검증", "spacing-variant", docs_with("배포검증"), "lexical: same docs as 배포검증")
add("deploy-ok 검증", "english-term-in-korean", docs_with_all("deploy-ok", "검증"), "lexical: docs whose body contains both deploy-ok and 검증")
add("캡틴", "2-char", task_titles_with("캡틴"), "lexical: task titles containing 캡틴")
add("stalldetect 블로커 감지", "identifier-plus-korean", ["doc:2005"], "curated: key reports/2026-09-17/d1-ev117-stalldetect-blockers")
add("NCP 이전 방향", "korean-phrase", ["doc:2658"], "curated: title of doc 2658")
add("fleet-ci-three-tiers", "english-key", ["doc:60861"], "curated: key decision/2026-11-07/fleet-ci-three-tiers-r2")
add("PA 두번째 self-hosted runner", "spacing-variant", ["doc:33375"], "curated: title of doc 33375")
add("바나나 우유 재고 정산", "no-answer", [], "no answer expected: phrase absent from corpus")
add("해저 케이블 절단 복구", "no-answer", [], "no answer expected: phrase absent from corpus")
add("김치냉장고 온도 센서 교정", "no-answer", [], "no answer expected: phrase absent from corpus")

assert len(Q) == 30, len(Q)
for q in Q:
    if not q["gold"] and q["category"] != "no-answer":
        sys.exit(f"empty gold for {q['text']}")
with open("queries.jsonl", "w", encoding="utf-8") as f:
    for q in Q:
        f.write(json.dumps(q, ensure_ascii=False) + "\n")
print("queries", len(Q), "with-gold", sum(1 for q in Q if q["gold"]), "no-answer", sum(1 for q in Q if not q["gold"]))
