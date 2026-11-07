# YouTrack 동기화 (`handoffkeep youtrack sync`)

YouTrack(`mgh3326.youtrack.cloud`, 프로젝트 `HK`)은 운영자의 화면이고, hk는
에이전트가 읽고 쓰는 기록 원본이다. 이 커맨드는 두 쪽을 맞춘다.

- hk → YouTrack: 만들거나 바뀐 작업, 새 hk 코멘트를 이슈로 민다.
- YouTrack → hk: `api/activitiesPage`를 커서로 읽어 운영자가 바꾼 상태·우선순위·
  레인·코멘트·새 이슈를 hk에 적용한다.
- 우선순위(`priority`)와 레인(`lane`)은 YouTrack 정본이다. hk는 값을 캐시해서
  내려주고, hk에서 온 쓰기는 YouTrack에 먼저 쓴다.

## 데이터 정본

| 데이터 | 정본 | hk가 하는 일 |
|---|---|---|
| 작업 번호·상태·refs·claim | hk | 그대로 |
| 우선순위·레인 | YouTrack | `youtrack_field_cache`에 캐시(기본 TTL 60초), 읽기 경로가 내려줌 |
| 운영자 코멘트 | YouTrack | hk 코멘트로 복제(작성자 `operator:youtrack`) |
| 한국어 제목·본문 | YouTrack | hk `translations`의 한국어 제목을 요약으로 씀 |

## 한 번의 실행

`handoffkeep youtrack sync`는 다음 순서로 **한 번에 제한된 양만** 처리한다.

1. 이전 실행에서 YouTrack 쓰기에 실패해 남은 되돌림(`pending reverts`)을 재시도한다.
2. 링크된 작업의 필드 캐시를 TTL 기준으로 갱신한다(`--refresh`, 기본 100건).
3. hk → YouTrack 밀기(`--limit`, 기본 50건).
   - 링크가 없는 미종료 작업은 이슈를 만든다(있으면 재사용).
   - 링크가 있고 `updated_at`이 마지막 푸시보다 새 작업은 필드를 갱신한다.
   - 아직 미러하지 않은 hk 코멘트를 단다(`--comments`, 작업당 기본 50건).
   - 요약은 한국어 번역이 있으면 그 값, 없으면 원래 제목이다. 본문 첫 줄은
     `https://console.robinco.dev/ui/tasks/<id>`이고 그 아래에 원래 제목이 온다.
4. YouTrack → hk 당기기(활동 `--activities`, 기본 100건). 커서 이후만 읽는다.
   - 작성자가 hk-bot이면 건너뛴다(미러가 쓴 활동).
   - `hk 상태` 변경 → `TransitionTaskWith`. 거부되면 YouTrack 필드를 hk 상태로
     되돌리고 `hk가 거부: <이유>` 코멘트를 단다. `merged`의 종료 근거는
     `refs.pr`을 쓴다.
   - `hk 우선순위`/`레인` 변경 → 캐시와 hk 컬럼을 갱신하고 `task_events`에
     `operator:youtrack` 행을 남긴다.
   - 코멘트 → hk 코멘트(작성자 `operator:youtrack`)로 복제하고 링크해서 다시
     밀지 않는다.
   - 운영자가 만든 새 이슈 → hk 작업(`triage`, 레인은 이슈의 `레인` 또는
     `operator-desk`)을 만들고 링크한다.
   - `/지시`, `/답`, `/결정` 코멘트는 아직 처리하지 않고 건너뛴 사유만 남긴다.

활동 하나는 `youtrack_processed_activities`에 먼저 기록하고 한 트랜잭션에서
적용한다. 커서를 되감거나 같은 페이지를 다시 읽어도 두 번 적용되지 않는다.

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

이름이 없으면 실행 중에 번들 값으로 추가한다. 레인·프로젝트·종류·등급·티어도
같은 방식으로 빠진 값을 추가한다.

## 환경 변수

| 변수 | 필수 | 뜻 |
|---|---|---|
| `HANDOFFKEEP_DB_URL` | sync | hk PostgreSQL 접속 문자열 |
| `YOUTRACK_URL` | sync, 서버 | `https://mgh3326.youtrack.cloud` |
| `YOUTRACK_BOT_API_KEY` | sync, 서버 | hk-bot 토큰. 절대 로그나 오류에 출력하지 않는다 |
| `YOUTRACK_PROJECT` | 아니오 | YouTrack 프로젝트(기본 `HK`) |
| `YOUTRACK_DEFAULT_HK_PROJECT` | 아니오 | 새 이슈에서 hk 프로젝트를 못 읽을 때(기본 `other`) |
| `YOUTRACK_DEFAULT_LANE` | 아니오 | 새 이슈에서 레인을 못 읽을 때(기본 `operator-desk`) |
| `YOUTRACK_CONSOLE_URL` | 아니오 | 이슈 본문 첫 줄 링크(기본 `https://console.robinco.dev`) |

서버(`handoffkeep serve`)는 `YOUTRACK_URL`과 `YOUTRACK_BOT_API_KEY`가 둘 다 있을
때만 읽기 캐시와 쓰기 통과를 켠다. 한쪽만 있으면 로그에
`youtrack.disabled reason=incomplete_config`를 남기고 기능을 끈다.

## 종료 코드

| 코드 | 뜻 |
|---|---|
| `0` | 이번 배치 성공 |
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

- **YouTrack 장애**: 읽기 API는 마지막 캐시 값을 `"stale": true`,
  `"priority_source": "youtrack"`로 내려준다. hk에서 온 우선순위·레인 쓰기는
  실패하고 hk는 바뀌지 않는다(`502 youtrack_unavailable`). sync는 종료 코드
  `1`로 끝나고 다음 실행이 이어받는다.
- **429/5xx**: 클라이언트가 `Retry-After`를 존중하며 최대 3번 재시도한다.
- **거부된 상태 변경**: 활동은 처리된 것으로 기록하고, 되돌림이 실패하면
  `youtrack_processed_activities.revert_done=false`로 남겨 다음 실행이 다시
  시도한다.
- **토큰 노출**: 토큰은 `Authorization` 헤더에만 넣고, 오류 문자열에서도
  `[redacted]`로 지운다.

## 끄는 방법

1. Dagu 작업을 멈춘다.
2. 서버에서 `YOUTRACK_URL`과 `YOUTRACK_BOT_API_KEY`를 뺀다. 서버를 재시작하면
   읽기 경로는 hk 컬럼을 그대로 쓰고, 쓰기 통과는 꺼진다.
3. 남은 링크·캐시 테이블은 그대로 둔다(다시 켜면 이어서 쓴다).

## 스키마

등록 마이그레이션 ID는 `20261107-youtrack-sync` 하나다. 새 테이블
(`youtrack_links`, `youtrack_cursor`, `youtrack_comment_links`,
`youtrack_field_cache`, `youtrack_processed_activities`)만 만들고 기존 행은
고치지 않는다. `tasks`/`task_comments`를 참조하는 외래 키를 만드는 동안 그 두
테이블에 `SHARE ROW EXCLUSIVE` 잠금이 잡히므로 `lock_timeout = 5s`로 기다린다.
