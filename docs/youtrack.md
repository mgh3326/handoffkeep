# YouTrack 동기화 (`handoffkeep youtrack sync`)

YouTrack(`mgh3326.youtrack.cloud`)은 운영자의 화면이고, hk는 에이전트가 읽고 쓰는
기록 원본이다. 이 커맨드는 두 쪽을 맞춘다. 프로젝트는 hk 프로젝트마다 YouTrack
프로젝트 하나로 대응한다(예: `handoffkeep`→`HK`, `panewire`→`PW`, 나머지는
`OTHER`).

- hk → YouTrack: 만들거나 바뀐 작업, 새 hk 코멘트를 이슈로 민다.
- YouTrack → hk: `api/activitiesPage`를 커서로 읽어 운영자가 바꾼 상태·우선순위·
  레인·코멘트·새 이슈를 hk에 적용한다.
- **PG 컬럼이 실행값이다.** 우선순위·레인 쓰기는 PG에 먼저 커밋되고 YouTrack으로는
  `youtrack_outbox`를 통해 나간다. YouTrack 장애가 hk 쓰기를 막지 않는다.

## 데이터 정본

| 데이터 | 정본 | hk가 하는 일 |
|---|---|---|
| 작업 번호·상태·우선순위·레인·refs·claim | hk (PG) | 그대로. 도구는 hk만 읽는다 |
| 운영자 코멘트 | YouTrack | hk 코멘트로 복제(작성자 `operator:youtrack`) |
| 한국어 제목·본문 | YouTrack | hk `translations`의 한국어 제목을 요약으로 씀 |
| 우선순위·레인 표시 캐시 | hk PG | `youtrack_field_cache`에 `source`/`fetched_at`/`stale`와 함께 저장 |

## 한 번의 실행

`handoffkeep youtrack sync`는 다음 순서로 **한 번에 제한된 양만** 처리한다.
당기기를 내보내기보다 먼저 하므로, 운영자가 방금 바꾼 값이 이번 실행에서
hk 값으로 덮이지 않는다.

1. 이전 실행에서 YouTrack 쓰기에 실패해 남은 되돌림(`pending reverts`)을 재시도한다.
2. YouTrack → hk 당기기(활동 `--activities`, 기본 100건). 커서 이후만 읽는다.
   - 작성자가 hk-bot이면 건너뛴다(미러가 쓴 활동).
   - `hk 상태` 변경 → `TransitionTaskWith`. 거부되면 YouTrack 필드를 hk 상태로
     되돌리고 `hk가 거부: <이유>` 코멘트를 단다. `merged`의 종료 근거는
     `refs.pr`을 쓴다.
   - `hk 우선순위`/`레인` 변경 → PG 컬럼과 캐시를 갱신하고 `task_events`에
     `operator:youtrack` 행을 남긴다.
   - 코멘트 → hk 코멘트(작성자 `operator:youtrack`)로 복제하고 링크해서 다시
     밀지 않는다.
   - 운영자가 만든 새 이슈 → hk 작업(`triage`, 레인은 이슈의 `레인` 또는
     `operator-desk`, 프로젝트는 이슈의 프로젝트 매핑)을 만들고 링크한다.
     매핑되지 않은 YouTrack 프로젝트의 이슈는 적용하지 않고 건너뛴다.
   - `/지시`, `/답`, `/결정` 코멘트는 아직 처리하지 않고 건너뛴 사유만 남긴다.
   - hk가 적용할 수 없는 활동(잘못된 레인·종류, 없는 hk 프로젝트 등)은
     건너뛴 것으로 기록하고 커서를 계속 진행한다. 한 활동이 페이지를 막지 않는다.
3. `youtrack_outbox`의 우선순위·레인 내보내기를 보낸다(`--outbox`, 기본 100건).
   실패하면 지수 백오프로 다시 예약하고, PG 값은 그대로 둔다. 내보내기가 값을
   보내는 사이 같은 행에 새 값이 들어오면, 방금 보낸 값의 완료·재예약은 그 행에
   적용되지 않고 새 값이 다음 실행에 나간다.
4. 미러된 작업의 필드 캐시를 오래된 순서로 갱신한다(`--refresh`, 기본 100건).
   TTL 만료로 읽기가 YouTrack을 부르지 않는다. 동기화가 캐시를 갱신한다.
5. hk → YouTrack 밀기(`--limit`, 기본 50건).
   - 미러가 없는 미종료 작업은 이슈를 만든다(`hk ID`로 기존 이슈를 먼저 찾는다).
   - 미러가 있고 `updated_at`이 마지막 동기화보다 새 작업은 필드를 갱신한다.
     내보내는 우선순위·레인은 캐시가 아니라 **PG 컬럼**에서 읽는다.
   - hk 프로젝트가 바뀌었으면 이슈를 그 프로젝트로 옮긴다. 이슈의 안정 ID는
     그대로라 링크가 유지되고 표시 번호만 바뀐다.
   - 아직 미러하지 않은 hk 코멘트를 단다(`--comments`, 작업당 기본 50건).
   - 요약은 한국어 번역이 있으면 그 값, 없으면 원래 제목이다. 본문 첫 줄은
     `https://console.robinco.dev/ui/tasks/<id>`이고 그 아래에 원래 제목이 온다.

활동 하나는 `youtrack_processed_activities`에 먼저 기록하고 한 트랜잭션에서
적용한다. 커서를 되감거나 같은 페이지를 다시 읽어도 두 번 적용되지 않는다.
작성자 필터와 활동 ID 멱등은 이후 웹훅·reconcile에도 같은 규칙으로 적용한다.

## 프로젝트 매핑

`youtrack_projects`가 `hk_project → YouTrack shortName / project id`를 가진다.
매핑이 없는 hk 프로젝트는 `OTHER`로 보낸다. 동기화는 프로젝트를 만들지 않는다;
매핑된 프로젝트가 YouTrack에 없으면 경고만 남기고 그 작업을 건너뛴다.

```sh
handoffkeep youtrack projects list
handoffkeep youtrack projects put handoffkeep HK
```

`put`은 YouTrack에서 shortName을 project id로 해석해 함께 저장한다.

코멘트 카나리(hk 3739)는 같은 매핑을 쓴다. `handoffkeep youtrack canary
add <작업>`은 `<shortName>-<작업 번호>` 이슈를 읽어 확인한 뒤 등록하고,
`handoffkeep youtrack ops`(별칭 `yt ops`)는 쓰기 작업 상태를 보여 준다. 쓰기
작업은 만들어질 때의 테넌트에 묶이므로 작업을 다른 테넌트로 다시 등록해도 이전
작업이 그쪽으로 가지 않는다. 확인과 등록 사이에 매핑이 바뀌면 등록이 거절된다. 자세한
규칙은 `docs/youtrack-write-protocol.md`. 레거시 미러 푸시는 쓰기 프로토콜이
소유한 코멘트를 다시 올리지 않는다.

## 상태 이름

YouTrack의 `hk 상태` 번들 값은 콘솔과 같은 한국어 이름이다.

| hk | YouTrack |
|---|---|
| `triage` | 분류 대기 |
| `backlog` | 대기 |
| `unstarted` | 착수 전 |
| `researching` | 조사 중 |
| `developing` | 개발 중 |
| `reviewing` | 검토 중 |
| `merged` | 머지됨 |
| `deployed` | 배포됨 |
| `completed` | 완료됨 |
| `dropped` | 종료됨 |
| `duplicate` | 중복 |
| `icebox` | 보류(보관) |

이름이 없으면 실행 중에 번들 값으로 추가한다. 레인·종류·등급·티어도 같은
방식으로 빠진 값을 추가한다. 필드 계약(이름과 타입)은
`internal/youtrack/schema.go` 한 파일에 있다.

## 환경 변수

| 변수 | 필수 | 뜻 |
|---|---|---|
| `HANDOFFKEEP_DB_URL` | sync, projects | hk PostgreSQL 접속 문자열 |
| `YOUTRACK_URL` | sync, 서버 | `https://mgh3326.youtrack.cloud` |
| `YOUTRACK_BOT_API_KEY` | sync, 서버 | hk-bot 토큰. 절대 로그나 오류에 출력하지 않는다 |
| `YOUTRACK_DEFAULT_HK_PROJECT` | 아니오 | 새 이슈에서 hk 프로젝트를 못 읽을 때(기본 `other`) |
| `YOUTRACK_DEFAULT_LANE` | 아니오 | 새 이슈에서 레인을 못 읽을 때(기본 `operator-desk`) |
| `YOUTRACK_CONSOLE_URL` | 아니오 | 이슈 본문 첫 줄 링크(기본 `https://console.robinco.dev`) |
| `HANDOFFKEEP_YT_COMMENTS_MODE` | 아니오 | `off`(기본) 또는 `canary`. 쓰기 프로토콜 코멘트 카나리(hk 3739, `docs/youtrack-write-protocol.md`). 다른 값이나 YouTrack 자격 없는 `canary`는 서버 시작 실패 |

서버(`handoffkeep serve`)는 `YOUTRACK_URL`과 `YOUTRACK_BOT_API_KEY`가 둘 다 있을
때만 읽기 캐시를 켠다. 한쪽만 있으면 로그에
`youtrack.disabled reason=incomplete_config`를 남기고 기능을 끈다. 서버는
우선순위·레인 쓰기를 PG에만 쓰고 outbox에 넣는다(YouTrack 호출 없음).

## 종료 코드

| 코드 | 뜻 |
|---|---|
| `0` | 이번 배치 성공(경고만 있으면 성공) |
| `1` | 일부 실패(리포트의 `errors` 참고). 다음 실행이 이어서 처리한다 |
| `2` | 환경 변수나 URL 설정 오류 |
| `3` | 다른 sync가 실행 중(단일 실행 잠금) |

## Dagu 작업

2분마다 한 번, 이전 실행이 끝난 뒤에만 시작한다. 같은 명령을 동시에 두 번
실행해도 두 번째는 종료 코드 `3`으로 끝나고 아무것도 쓰지 않는다.

```yaml
steps:
  - name: youtrack-sync
    command: handoffkeep youtrack sync
    env:
      - HANDOFFKEEP_DB_URL: ${HANDOFFKEEP_DB_URL}
      - YOUTRACK_URL: ${YOUTRACK_URL}
      - YOUTRACK_BOT_API_KEY: ${YOUTRACK_BOT_API_KEY}
```

## 실패했을 때

- **YouTrack 장애**: hk 쓰기는 그대로 성공하고 outbox가 재시도한다. 읽기 API는
  마지막 캐시 값을 `source`/`fetched_at`과 함께 내려주고, 갱신에 실패했으면
  `"stale": true`를 붙인다. sync는 종료 코드 `1`로 끝나고 다음 실행이 이어받는다.
- **429/5xx**: 클라이언트가 `Retry-After`를 존중하며 최대 3번 재시도한다.
- **거부된 상태 변경**: 활동은 처리된 것으로 기록하고, 되돌림이 실패하면
  `youtrack_processed_activities.revert_done=false`로 남겨 다음 실행이 다시
  시도한다.
- **프로젝트 없음/매핑 없음**: 경고만 남기고 그 작업을 건너뛴다. 동기화는
  프로젝트를 만들지 않는다.
- **토큰 노출**: 토큰은 `Authorization` 헤더에만 넣고, 오류 문자열에서도
  `[redacted]`로 지운다.

## 끄는 방법

1. Dagu 작업을 멈춘다.
2. 서버에서 `YOUTRACK_URL`과 `YOUTRACK_BOT_API_KEY`를 뺀다. 서버를 재시작하면
   읽기 경로는 hk 컬럼을 그대로 쓴다(쓰기는 원래부터 PG 우선이다).
3. 남은 미러·캐시·outbox는 그대로 둔다(다시 켜면 이어서 쓴다).

## 스키마

등록 마이그레이션 ID는 `20261107-youtrack-sync` 하나다. 새 테이블
(`youtrack_projects`, `youtrack_outbox`, `youtrack_cursor`,
`youtrack_comment_links`, `youtrack_field_cache`,
`youtrack_processed_activities`)을 만들고, `task_origins`에 `tenant`, `role`,
`synced_at` 열과 `youtrack` provider를 추가한다. 미러 링크는 별도 테이블이 아니라
`task_origins`의 한 행이다(provider `youtrack`, namespace=tenant, 안정 ID=이슈
내부 id, 표시 번호는 `display_ref`).

`task_origins` 변경은 그 테이블에 `ACCESS EXCLUSIVE`를 잡고, `tasks`를 참조하는
외래 키를 만드는 동안 `tasks`에 `SHARE ROW EXCLUSIVE`를 잡는다. `lock_timeout =
5s`로 기다린다.
