# DBCat

가볍고 빠른 데스크톱 DB 클라이언트입니다. Wails + htmx, SQLite · PostgreSQL · MySQL/MariaDB를 지원합니다.

## 실행

```bash
open build/bin/DBCat.app          # 이미 빌드된 앱 실행
```

처음 실행하면 연결이 없습니다. 사이드바 위쪽의 **새 연결** 버튼을 눌러 샘플 DB를 추가하세요.

| 항목 | 값 |
|---|---|
| 연결 유형 | SQLite |
| 연결 이름 | 데모 |
| 데이터베이스 파일 | `/Volumes/work/DBCat/sample/demo.db` (`찾아보기…`로 선택 가능) |

## 한 번씩 눌러 볼 것

| 기능 | 방법 |
|---|---|
| 트리 탐색 | 연결 더블클릭 → `main` 더블클릭 → 테이블 / 뷰 목록 |
| 데이터 보기 | 테이블 더블클릭 (`customers` 1,200행 · 페이징, 헤더 클릭 정렬, 필터창에 `city = '서울' AND vip = 1`) |
| 셀 편집 | 셀 더블클릭 → Enter 저장 · Esc 취소 · `⌘/Ctrl+0` = NULL |
| 행 추가 / 삭제 | 툴바 `행 추가`, 행 번호 클릭 후 `행 삭제` |
| 내보내기 | 툴바 `CSV` / `JSON` (현재 필터·정렬이 적용된 전체 행) |
| 테이블 설계 | 테이블 우클릭 → `테이블 설계` (필드·인덱스 추가/삭제 → `SQL 미리보기` → `저장`) |
| 새 테이블 | 데이터베이스 우클릭 → `새 테이블…` |
| SQL 에디터 | `새 쿼리` — 자동 완성(`customers.` 입력), `⌘/Ctrl+Enter` 실행, 선택한 부분만 실행 |
| 여러 문장 | `SELECT 1; SELECT 2;` 처럼 `;` 로 구분하면 문장별 결과 탭 (오류 시 중단) |
| 쿼리 기록 | 에디터 툴바 `기록` — 검색, 가져오기, 삭제, 기록 끄기 |
| 트리거 확인 | `UPDATE orders SET status='cancelled' WHERE id=1;` 후 `SELECT * FROM audit_log;` |
| 오브젝트 창 | 데이터베이스 더블클릭 → 우측 상단 `목록 / 자세히 / 아이콘 / ER 다이어그램` 전환, `이름 필터`, 하단 정보 창의 `일반` · `DDL` 탭 (오브젝트 클릭). 위쪽 탭으로 `테이블·뷰 / 인덱스 / 트리거 / 함수`(MySQL·MariaDB는 프로시저 포함) 전환 |
| 인덱스·트리거·함수 | 트리에서 `인덱스` · `트리거` · `함수` 그룹을 펼침 (개수 표시) → 더블클릭 또는 우클릭 `정의 보기` / `쿼리 편집기로 열기` |
| 폼 보기 | 테이블 데이터 툴바 `폼 보기` — 한 행을 세로 폼으로 보고 편집 (이전/다음 행 이동, `NULL` 체크) |
| 백업 (덤프) | 데이터베이스(PostgreSQL은 스키마) 우클릭 → `백업 (SQL 덤프)…` — 테이블 선택, 구조/데이터, `DROP … IF EXISTS`, 뷰, 트리거·함수·프로시저 선택 후 `.sql` 저장 |
| 복원 (SQL 실행) | 데이터베이스(스키마) 우클릭 → `SQL 파일 실행 (복원)…` — `.sql` 파일 선택 → 확인 → 문장 수 / 오류 요약 (기본은 첫 오류에서 중단, `오류가 나도 계속 실행` 선택 가능). 최대 256MB |
| ER 다이어그램 | 보기 모드 `ER` — 박스 드래그, 휠 줌, 배경 드래그 이동, `맞춤` / `재배치`, `테이블 찾기`, 박스 더블클릭으로 열기, `SVG` / `PNG` 저장 (위치는 기억됩니다) |
| 외래 키 | 테이블 설계 → `외래 키` 탭 (PostgreSQL/MySQL은 추가·삭제, SQLite는 새 테이블 생성 때만 정의 가능) |
| SQL 포맷 / 주석 | 에디터 `포맷` 버튼 또는 `Alt+Shift+F` (선택 영역 우선), `⌘/Ctrl+/` 주석 토글 |
| 셀 값 보기 | 셀 우클릭 → `상세 보기…` (긴 텍스트, JSON 정렬), 읽기 전용 그리드는 더블클릭. 헤더 경계 드래그로 컬럼 너비 조절 (더블클릭 = 내용에 맞춤) |
| 명령 팔레트 | `⌘K` (Windows·Linux는 `Ctrl+K`) — 새 연결, 새 쿼리, 테마 전환 등을 검색해서 실행 |
| 테마 | 헤더 오른쪽 달/해 버튼 (설정이 없으면 시스템 설정을 따릅니다) |

## 다른 DB에 연결

- **PostgreSQL / MySQL / MariaDB**: 호스트·포트·사용자·비밀번호 입력 후 `연결 테스트`. 비밀번호는 macOS 키체인에 저장됩니다.
- **SSL/TLS**: 연결 다이얼로그의 `SSL / TLS` (인증서 검증 모드는 CA 파일 지정 가능).
- **SSH 터널**: `SSH 터널`을 체크하고 호스트·사용자·비밀번호 또는 개인키를 입력합니다.
  호스트 키는 `~/.ssh/known_hosts` 로 검증하며, 처음 보는 호스트는 지문을 확인한 뒤 앱에서 바로 등록할 수 있습니다.
  호스트 키가 바뀐 서버는 항상 거부합니다.

로컬에서 PostgreSQL을 빠르게 띄워 보려면:

```bash
docker run -d --name pg-try -e POSTGRES_PASSWORD=pw -p 127.0.0.1:5432:5432 postgres:17-alpine
# 연결: 호스트 127.0.0.1, 포트 5432, 사용자 postgres, 비밀번호 pw
docker rm -f pg-try                 # 끝나면 정리
```

## 저장 위치

| 파일 | 내용 |
|---|---|
| `~/Library/Application Support/dbcat/connections.json` | 연결 목록 (비밀번호 제외, 0600) |
| `~/Library/Application Support/dbcat/history.json` | 쿼리 기록 (0600) |
| macOS 키체인, 서비스 `dbcat` | DB / SSH 비밀번호 |

## 개발

```bash
go install github.com/wailsapp/wails/v2/cmd/wails@v2.17.1   # 최초 1회
wails build                                                   # build/bin/DBCat.app
go run . -web 127.0.0.1:8080                                  # 창 없이 브라우저로 (루프백만 허용)
go test ./...                                                 # 단위 테스트

# 에디터 번들을 수정했을 때 (web-src/editor)
cd web-src/editor && npm install && npm run build
```

실제 서버가 필요한 통합 테스트는 환경변수가 있을 때만 실행됩니다. 변수 목록은 `internal/db/integration_test.go` 상단 주석을 보세요.

## 릴리스

`v`로 시작하는 태그를 push하면 GitHub Actions(`.github/workflows/release.yml`)가 테스트 후 세 플랫폼을 빌드하고 Release를 만듭니다.

```bash
git tag v0.1.0 && git push origin v0.1.0     # 태그에 `-`가 있으면(v0.2.0-rc1) 프리릴리스
```

| 플랫폼 | 산출물 | 비고 |
|---|---|---|
| macOS | `DBCat-<버전>-macos-universal.zip` (Apple Silicon + Intel) | 공증(notarization)이 없어 처음 실행할 때 우클릭 → 열기가 필요합니다 |
| Windows | `DBCat-<버전>-windows-amd64.zip` | WebView2 런타임 필요 (Windows 11은 기본 포함) |
| Linux | `DBCat-<버전>-linux-amd64.tar.gz` | `libgtk-3-0`, `libwebkit2gtk-4.1-0` 필요 |

Release에는 `SHA256SUMS.txt`도 함께 올라갑니다. Actions 탭의 `release` → `Run workflow`로 Release 없이 빌드만 시험할 수 있습니다.

## 제약

- 백업은 `pg_dump` / `mysqldump` 없이 서버에서 직접 만든 이식 가능한 SQL 스크립트입니다. 다음은 덤프하지 않습니다.
  - PostgreSQL: enum 등 사용자 정의 타입, 확장(extension), 권한(GRANT), 시퀀스 옵션(시작값·증가값 등; 값은 `setval`로 맞춥니다), 소유자
  - MySQL / MariaDB: 이벤트, 권한, `DEFINER`(제거되어 복원하는 사용자 소유가 됩니다), 공간(geometry) 데이터 값의 정확한 표현
  - SQLite: 테이블 · 인덱스 · 뷰 · 트리거(원본 `CREATE` 문 그대로)
- 복원은 파일의 문장을 순서대로 실행할 뿐이므로 `DROP` / `DELETE`가 들어 있으면 기존 데이터가 사라집니다 (실행 전 확인창이 한 번 뜹니다). MySQL의 `DELIMITER` 구문은 지원합니다.
- 필드 유형/NULL/기본값 변경은 PostgreSQL만 지원합니다 (SQLite, MySQL은 이름 변경·추가·삭제만).
- 결과는 문장당 1,000행까지 표시합니다 (테이블 데이터 보기와 내보내기는 전체).
- 서드파티 라이선스: `internal/web/static/THIRD_PARTY.txt`

## 라이선스

[MIT License](LICENSE). 서드파티 라이선스는 다음 파일에 있습니다.

| 대상 | 파일 |
|---|---|
| 바이너리에 링크된 Go 모듈 (26개) | [THIRD_PARTY_LICENSES.txt](THIRD_PARTY_LICENSES.txt) |
| UI에 번들된 JavaScript (htmx, CodeMirror, dagre 등) | `internal/web/static/THIRD_PARTY.txt` |
