# YouTrack 연동

YouTrack(`mgh3326.youtrack.cloud`)은 운영자의 화면이고, hk는 에이전트가 읽고 쓰는
기록 원본이다. hk가 YouTrack에 쓰는 경로는 **쓰기 프로토콜** 하나다. 작업·필드·
코멘트·문서·결정·이동은 hk 쓰기와 같은 트랜잭션에서 op로 남고, 워커가 그 op를
YouTrack에 보낸다. 프로토콜의 규칙, 상태 기계, 자동 복구는
`docs/youtrack-write-protocol.md`가 정본이다.

이 문서는 운영자가 쓰는 설정과 커맨드만 다룬다.

## 프로젝트 매핑

`youtrack_projects`가 `hk_project → YouTrack shortName / project id`를 가진다.
매핑이 없는 hk 프로젝트는 `OTHER`로 보낸다. 매핑 작업은 프로젝트를 만들지 않는다.

```sh
handoffkeep youtrack projects list
handoffkeep youtrack projects put handoffkeep HK
```

`put`은 YouTrack에서 shortName을 project id로 해석해 함께 저장한다. 매핑이
YouTrack에 없는 프로젝트를 가리키면 그 작업은 건너뛰고 경고만 남긴다.

## 점검과 상태

```sh
handoffkeep youtrack ops [--state S] [--kind K] [--task N]   # 쓰기 작업 상태 (별칭 yt ops)
handoffkeep youtrack canary add|remove|list                  # 코멘트 카나리 등록
handoffkeep youtrack audit [--project P] [--task N] [--enqueue] [--limit N]
handoffkeep youtrack authority show                          # 종류별 정본(hk 또는 youtrack)
handoffkeep youtrack authority set <kind|all> <hk|youtrack> --by NAME --note TEXT
```

`authority set`은 종류 하나의 정본을 바꾼다. `youtrack`으로 바꾼 종류는 hk 쓰기가
그 종류를 YouTrack에 보내지 않고, `audit`이 YouTrack에서 PG로 고친다.

## 백필

백필 커맨드는 이미 있는 hk 데이터를 YouTrack 쪽에 맞춘다. fields-backfill은
`HANDOFFKEEP_YT_FIELDS_MODE=on`이 아니면 아무것도 쓰지 않고 종료 코드 `6`으로 끝난다.

```sh
handoffkeep youtrack comments-preflight [--project P] [--days N] [--all]
handoffkeep youtrack comments-backfill  [--project P] [--days N] [--all] [--by NAME]
handoffkeep youtrack articles-preflight [--project P]
handoffkeep youtrack articles-backfill  [--project P] [--by NAME]
handoffkeep youtrack issues-backfill    [--project P] [--dry-run]
handoffkeep youtrack fields-backfill    [--project P] [--task N] [--limit N] [--dry-run]
handoffkeep youtrack decisions-backfill [--apply] [--by NAME] [--limit N]
```

## 스키마 (`youtrack schema`)

YouTrack 쪽 필드·번들·프로젝트의 계약은 `internal/youtrack/schema.go`의 `Desired`
한 곳에 선언하고, `plan`과 `apply`가 같은 선언을 읽는다. 둘 다 서버 컨테이너에서
`HANDOFFKEEP_DB_URL`, `YOUTRACK_URL`, `YOUTRACK_BOT_API_KEY`로 돈다.

```sh
handoffkeep youtrack schema plan  [--project P] [--json]
handoffkeep youtrack schema apply [--project P] [--json]
```

`plan`은 아무것도 쓰지 않고 변경을 한 줄씩(탭으로 구분: hk 프로젝트, 대상,
동작, 값) 낸다. YouTrack 전체에 걸린 변경은 프로젝트 칸이 `-`다. `report` 줄은
`apply`가 고칠 수 없는 점검 결과다(내장 링크 타입 Subtask·Relates·Duplicate가
없거나 방향이 다름, 같은 이름의 필드가 다른 타입이거나 번들이 없음, `영역`과
`영역_0`처럼 `<이름>_<숫자>`가 기본 이름과 함께 붙은 중복 필드). 이런 필드는
건드리지 않는다. 링크 타입은 만들지 않는다.

`apply`는 같은 계획을 프로젝트 → 필드 정의 → 필드 연결 → 번들 값 → 플래그·ordinal
순서로, 한 번에 요청 하나씩 실행한다. 변경마다 `youtrack.schema_applied` 로그를
남기며, 첫 오류에서 멈춘다(`youtrack.schema_apply_failed`). 429는 서버의
`Retry-After` 전체(실행 제한 15분 안)만큼 기다린 뒤 다시 보내고, 기다림이 실행
제한을 넘으면 429를 그대로 돌려주며 멈춘다. 바로 이어서 `apply`를 다시 돌리면 계획이
비어 있다.

| 대상 | 하는 일 |
|---|---|
| `hk 상태` | 12개 상태 값을 모두 두고, 각 값의 `isResolved`와 ordinal을 hk 상태 순서로 맞춘다 |
| `종류`, `등급`, `티어`, `레인` | 없는 값만 추가한다(종류는 저장소의 닫힌 집합, 등급 S+·S·A+·A·B·C, 티어 T0–T3, 레인은 hk 작업에 쓰인 모든 레인) |
| `마감`, `영역`, `마일스톤`, `예상 소요` | 필드 정의가 없으면 만들고, 프로젝트에 없으면 연결한다 |
| `hk ID`, `hk 우선순위`, `hk 프로젝트`, `PR`, `작업 job`, `멈춤` | 위와 같이 정의를 만들고 연결한다 |
| 프로젝트 | hk 프로젝트마다 YouTrack 프로젝트와 `youtrack_projects` 행이 있게 한다. 같은 이름의 YouTrack 프로젝트가 있으면 매핑만 쓰고, 없으면 만든다. HK 버킷·SHRDOC·DEC는 건드리지 않는다 |

값·필드·번들·프로젝트를 지우거나 이름을 바꾸지 않고, 필드를 떼지 않는다.
`영역_0`·`마일스톤_0`처럼 기본 필드 이름에 `_<숫자>`가 붙은 필드는 `report` 줄로만
알리고, 지우거나 이름을 바꾸지 않는다. 프로젝트별 번들을 유지한다(공유 번들로
바꾸지 않는다).

## 검색

`HANDOFFKEEP_SEARCH_BACKEND=youtrack`이면 서버가 `/v1/search`의 작업·문서 결과를
YouTrack에 묻는다. 정확 일치 조회와 PostgreSQL 결과는 그대로 PG가 답한다.
YouTrack 호출이 실패하면 PostgreSQL 결과로 돌아가고 이유를 로그에 남긴다.

이 선택자는 `YOUTRACK_URL`과 `YOUTRACK_BOT_API_KEY`가 모두 있을 때만 적용된다.
없으면 서버는 `search.backend youtrack ignored reason=youtrack_credentials_unset`을
한 줄 남기고 PostgreSQL로 검색한다. 자격 값은 로그에 남기지 않는다.

## 환경 변수

| 변수 | 필수 | 뜻 |
|---|---|---|
| `HANDOFFKEEP_DB_URL` | 프로젝트·스키마·백필·점검 | hk PostgreSQL 접속 문자열 |
| `YOUTRACK_URL` | YouTrack을 쓰는 커맨드와 서버 | `https://mgh3326.youtrack.cloud` |
| `YOUTRACK_BOT_API_KEY` | YouTrack을 쓰는 커맨드와 서버 | hk-bot 토큰. 로그나 오류에 출력하지 않는다 |
| `HANDOFFKEEP_SEARCH_BACKEND` | 아니오 | `youtrack`이면 검색을 YouTrack에 묻는다. 그 밖의 값은 PostgreSQL |
| `HANDOFFKEEP_SEARCH_YOUTRACK_TIMEOUT` | 아니오 | YouTrack 검색 한 번의 상한(Go 기간 문자열) |
| `HANDOFFKEEP_YT_COMMENTS_MODE` | 아니오 | `off`(기본), `canary` 또는 `on`. 코멘트 쓰기 |
| `HANDOFFKEEP_YT_COMMENTS_CATCHUP` | 아니오 | `on`이면 코멘트 catch-up 패스를 돈다(기본 `off`) |
| `HANDOFFKEEP_YT_ISSUES_MODE` | 아니오 | `on`이면 작업 이슈를 만든다 |
| `HANDOFFKEEP_YT_FIELDS_MODE` | 아니오 | `on`이면 작업 필드(상태·우선순위·레인 등)를 쓴다 |
| `HANDOFFKEEP_YT_MOVES_MODE` | 아니오 | `on`이면 프로젝트 이동을 쓴다. `FIELDS_MODE=on`이어야 한다 |
| `HANDOFFKEEP_YT_DOCS_MODE` | 아니오 | `on`이면 문서를 YouTrack 글(article)로 쓴다 |
| `HANDOFFKEEP_YT_DOC_COMMENTS_MODE` | 아니오 | `on`이면 문서 코멘트를 쓴다 |
| `HANDOFFKEEP_YT_DECISIONS_MODE` | 아니오 | `on`이면 결정 요청을 DEC 프로젝트 이슈로 쓴다 |

`HANDOFFKEEP_YT_COMMENTS_MODE`만 `canary`를 받는다. 나머지 모드는 `off`나 `on`만
받고, 다른 값은 시작하거나 커맨드를 실행할 때 오류다. 각 모드의 쓰기는 YouTrack
자격(`YOUTRACK_URL`, `YOUTRACK_BOT_API_KEY`)이 있어야 시작한다.

## 종료 코드

| 코드 | 뜻 |
|---|---|
| `0` | 성공 |
| `1` | 실패(리포트나 로그 참고). 다시 실행하면 이어서 처리한다 |
| `2` | 환경 변수나 URL 설정 오류 |
| `6` | fields-backfill을 `HANDOFFKEEP_YT_FIELDS_MODE=on` 없이 실행함. 아무것도 쓰지 않았다 |

## 실패했을 때

- **YouTrack 장애**: hk 쓰기는 그대로 성공하고, 쓰기 op가 재시도한다. 상태는
  `youtrack ops`에서 본다.
- **429/5xx**: 클라이언트가 `Retry-After`를 존중하며 재시도한다.
- **프로젝트 없음/매핑 없음**: 경고만 남기고 그 작업을 건너뛴다. 프로젝트는 만들지
  않는다.
- **토큰 노출**: 토큰은 `Authorization` 헤더에만 넣고, 오류 문자열에서도
  `[redacted]`로 지운다.

## 테이블

쓰기 프로토콜은 `yt_*` 테이블과 `task_origins`(미러 링크), `youtrack_projects`,
`youtrack_comment_links`, `youtrack_decision_mirrors`, `youtrack_doc_articles`를
쓴다. 옛 동기화 세대가 쓰던 `youtrack_outbox`, `youtrack_field_cache`,
`youtrack_cursor`, `youtrack_attempts`, `youtrack_processed_activities`는 더 이상
읽거나 쓰지 않는다. 테이블은 빈 채로 남겨 두고, 나중의 마이그레이션이 지운다.
