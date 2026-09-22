# photos-to-drive

[English](README.md)

macOS向けGo CLI。Apple「写真」ライブラリ内に実在する原本を **1ファイルずつ一時コピー → Google Driveへ転送 → サイズとMD5を照合 → 一時コピー削除** します。

**元の写真ライブラリ・iCloud設定・iPhoneの写真は変更しません。元のライブラリの容量を減らすツールではありません。**

## 対象と制約

- 現行ライブラリの `originals/` 以下のローカル画像・動画を読み取ります。PhotoKitや写真アプリの「書き出す」操作を使う実装ではありません。対応する拡張子は `main.go` の `media` にあります。
- iCloudの一覧・原本の有無を問い合わせません。ローカルにない原本は対象になりません。macOSのdatalessフラグがあるファイルは読みません。
- 内部のファイル名（UUID等）で保存します。撮影時の埋め込みEXIFなどはバイト単位で保持しますが、写真アプリ内のアルバム・人物・お気に入り・編集・表示上の元ファイル名は取得しません。
- Live Photosの画像と動画、RAWとJPEGはそれぞれ別ファイルです。Google Drive上でAppleのLive Photosとして再構成はしません。
- DB上の削除状態・非表示状態を参照しないため、「最近削除した項目」などの原本が `originals/` に残っていれば対象に含まれる可能性があります。参照形式でライブラリ外に置いた原本は対象外です。
- したがって「写真アプリ内の全写真を完全にバックアップした」ことを保証するものではありません。これだけを根拠にライブラリ全体を削除しないでください。
- `originals/` がない古いライブラリは明示的にエラーとします。処理中は「写真」アプリを終了し、ライブラリの整理や移動を避けてください。

## 必要なもの

- macOS、Go 1.24以上
- [rclone](https://rclone.org/install/) とGoogle Drive remote設定（転送処理に使用）
- ライブラリへの読み取り権限。`Operation not permitted` の場合は「システム設定 → プライバシーとセキュリティ → フルディスクアクセス」で実行元のTerminal等を許可し、再起動してください。

```sh
# Homebrewを利用している場合
brew install rclone
```

Homebrewがない場合は、rclone公式のビルド済みバイナリを `sudo` なしで導入できます。次のコマンドはAppleシリコン版を `~/.local/bin` に配置します。

```sh
mkdir -p "$HOME/.local/bin"
cd "$HOME/Downloads"
curl -O https://downloads.rclone.org/rclone-current-osx-arm64.zip
unzip -a rclone-current-osx-arm64.zip
cp rclone-*-osx-arm64/rclone "$HOME/.local/bin/rclone"
chmod 755 "$HOME/.local/bin/rclone"
"$HOME/.local/bin/rclone" version
```

Intel Macでは、上記2か所の `arm64` を `amd64` に置き換えてください。`uname -m` の結果が `arm64` ならAppleシリコン、`x86_64` ならIntelです。ブラウザからのダウンロードやシステム全体への導入方法は、[rclone公式インストールガイド](https://rclone.org/install/)でも確認できます。

## Google Driveへの接続設定

rcloneの共有Google OAuthクライアントは2026年中に廃止予定です。remoteを設定する前に、自分専用のOAuthクライアントを作成します。詳細は[rclone公式「Making your own client ID」](https://rclone.org/drive/#making-your-own-client-id)を参照してください。

Google Cloud Consoleで次の設定を行います。

1. プロジェクトを作成または選択し、**Google Drive API**を有効にします。
2. **Google Auth Platform**を開き、対象をExternalとして設定します。アプリがテスト中の場合は、転送先Driveを所有するGoogleアカウントをテストユーザーに追加します。テスト中の認証は7日後に失効します。
3. 「データアクセス」に `https://www.googleapis.com/auth/drive.file` を追加します。この権限なら、rcloneが作成したファイルへアクセスでき、Drive内の既存ファイル全体への権限は与えません。転送先フォルダはrcloneに作成させてください。Google Driveの画面で手作業で作ったフォルダは、この権限では見えない場合があります。詳細は[Google公式のDrive権限ガイド](https://developers.google.com/workspace/drive/api/guides/api-specific-auth)を参照してください。
4. アプリケーションの種類を**デスクトップアプリ**としてOAuthクライアントを作成し、Client IDとClient Secretを控えます。これらを公開したり、リポジトリへコミットしたりしないでください。

次にrcloneを設定します。`~/.local/bin` に導入した場合は次のフルパスを使います。PATHが通っている場合は `rclone config` でも構いません。

```sh
"$HOME/.local/bin/rclone" config
```

次のように回答します。番号はrcloneのバージョンで変わるため、記載した文字列を入力できる項目では文字列を使います。

| 質問 | 入力内容 |
| --- | --- |
| 新しいremoteを作成 | `n` |
| `name>` | `gdrive` |
| `Storage>` | `drive`（Google Cloud StorageやGoogle PhotosではなくGoogle Drive） |
| `client_id>` | デスクトップアプリのClient ID |
| `client_secret>` | デスクトップアプリのClient Secret |
| `scope>` | `drive.file`、または画面に表示された対応番号 |
| `service_account_file>` | 空欄のままEnter |
| 高度な設定を編集するか | `n` |
| ブラウザで認証するか | `y` |
| Shared Driveとして設定するか | Google Workspaceの共有ドライブを明示的に使う場合以外は`n` |
| remoteを保存するか | `y` |

ブラウザでは、テストユーザーに追加したものと同じGoogleアカウントでログインし、アクセスを許可します。**「アクセスをブロック」**と表示される場合は、通常、そのアカウントが **Google Auth Platform → 対象（Audience）→ テストユーザー** に追加されていないか、OAuth Client IDを作成したCloudプロジェクトとは別のプロジェクトを編集しています。

このCLIを実行する前に、remoteの保存と認証を確認します。

```sh
"$HOME/.local/bin/rclone" listremotes
"$HOME/.local/bin/rclone" lsd "gdrive:"
"$HOME/.local/bin/rclone" about "gdrive:"
```

`listremotes` に `gdrive:` が表示される必要があります。まだrcloneがフォルダを作っていない場合、`lsd` は何も表示しないことがありますが、認証エラーなしで終了すれば問題ありません。`about` ではDriveの容量が表示されます。

rcloneはClient IDとOAuthトークンを `~/.config/rclone/rclone.conf` に保存します。このCLIの `state.json` にはコピーしません。`rclone.conf` は秘密情報として扱い、共有やコミットをしないでください。`didn't find section in config file ("gdrive")` と表示された場合は、設定が最後まで保存されていないか、remoteを別の名前で保存しています。

`~/.local/bin` に入れたrcloneへPATHが通っていない場合は、CLIに場所を指定します。

```sh
./photos-to-drive \
  --rclone "$HOME/.local/bin/rclone" \
  --library "$HOME/Pictures/写真ライブラリ.photoslibrary" \
  --remote 'gdrive:MacPhotos' \
  --execute --limit 3
```

## 転送状況の確認

Google Drive側のサイズとMD5が一致したファイルだけを、状態ファイルへ記録します。Google Driveへ接続したり変更したりせず、現在の集計を確認できます。

```sh
go run . \
  --library "$HOME/Pictures/写真ライブラリ.photoslibrary" \
  --status
```

対象となる全ローカルパスと状態をCSVへ出力する場合は、次のように実行します。

```sh
go run . \
  --library "$HOME/Pictures/写真ライブラリ.photoslibrary" \
  --status \
  --report transfer-status.csv
```

状態の意味は次のとおりです。

- `verified`：サイズと更新時刻が、転送・MD5検証に成功した記録と現在も一致しています。
- `pending`：転送成功の記録がありません。
- `changed_since_verification`：転送記録後にローカルファイルが変化したため、再処理が必要です。
- `source_missing_after_verification`：転送記録はありますが、現在のローカル一覧には元パスがありません。

この表示はローカルの状態記録に基づきます。転送後にGoogle Drive側で手動削除されたファイルまでは再照会しません。写真ライブラリを削除する前の最終確認では、新しいworkディレクトリを指定して再実行してください。Drive上で一致する既存ファイルは残したまま検証し、欠けているものは再アップロードします。

```sh
go run . \
  --library "$HOME/Pictures/写真ライブラリ.photoslibrary" \
  --remote 'gdrive:MacPhotos' \
  --work .photos-to-drive-final-audit \
  --execute
```

`originals/`内のUUIDパスを、「写真」アプリ上の項目、元ファイル名、タイトルへ対応付けるレポートも作成できます。

```sh
go run . \
  --library "$HOME/Pictures/写真ライブラリ.photoslibrary" \
  --photos-report photos-transfer-status.csv
```

メタデータの読み取り中は「写真」アプリが開きます。初回はmacOSからTerminalによる「写真」の操作許可を求められる場合があります。**システム設定 → プライバシーとセキュリティ → オートメーション**で許可してください。Live Photoの静止画と動画は1つの写真項目にまとめ、対応する全ローカルリソースが検証済みの場合だけ`verified`にします。ほかに`partially_verified`、`pending`、`changed_since_verification`、`not_found_in_photos`などがあります。

対応付けには、「写真」のローカル識別子と`originals/`内のファイル名に共通するasset UUIDを使います。`not_found_in_photos`や`unmatched_local_filename`の行は、安全に削除できると判断せず個別に確認してください。

`写真ライブラリ.photoslibrary/originals` 内のファイルを個別に削除しないでください。写真ライブラリを壊す可能性があります。現時点では、このCLIから写真項目を削除する機能は意図的に追加していません。Appleの正式な写真APIで削除すると「最近削除した項目」へ移動しますが、iCloud写真がONの場合、その削除はiCloudや同期中のiPhoneにも反映されます。このレポートで全件完了を確認した後、希望するiCloudの挙動に合う別の削除手順を選びます。

## ビルド・実行

```sh
cd ~/work/go/photos-to-drive
go build -o photos-to-drive .

# プレビュー（ライブラリ名はFinderで確認。ドラッグ＆ドロップでも指定できます）
./photos-to-drive --library "$HOME/Pictures/写真ライブラリ.photoslibrary"

# 最初に3ファイルだけ試す
./photos-to-drive \
  --library "$HOME/Pictures/写真ライブラリ.photoslibrary" \
  --remote 'gdrive:MacPhotos' \
  --execute --limit 3

# 同じ指定で続きを転送
./photos-to-drive \
  --library "$HOME/Pictures/写真ライブラリ.photoslibrary" \
  --remote 'gdrive:MacPhotos' \
  --execute
```

名前の濁点がmacOS上で別のUnicode表現になっていることがあります。パスが見つからない場合はFinderから実際のパスをドラッグして入力してください。

- `--execute` がなければ一覧表示のみ。認証、アップロード、一時コピーは行いません。
- デフォルトで `.photos-to-drive/` に状態と一時コピーを置きます。別の場所なら `--work /path/to/work` を指定します。ライブラリ内は指定できません。
- 一時コピーは最大1ファイル分です。空き容量が2GiB以下になると安全に停止し、その余裕を残したまま収まらない大きなファイルだけは個別にスキップします。`STOP`と`SKIP`には、ファイルサイズ、現在の空き容量、安全のため残す容量、stagingに使用できる容量を表示するため、原因を判別できます。どちらの場合も終了コード1になります。容量を空けて同じコマンドを再実行すれば、記録済みの続きから処理します。しきい値は `--reserve-bytes` で変更できますが、空き容量が少ない状態で下げることは推奨しません。
- `--limit` はその実行で新たに処理するファイル数の上限です。画像と動画はそれぞれ1ファイルです。
- Ctrl+Cで停止可能。コピー途中は破棄、転送・検証の失敗では一時コピーを残します。次回起動時は専用stagingの残りを削除して元ファイルから再試行します。
- 転送先は `remote/ライブラリパス識別子/原本のサブフォルダ/MD5-内部ファイル名`。内容が変わった場合は別名になり、以前の内容は残ります。`rclone copyto --immutable` を利用し、既存の異なる内容を上書きしません。
- 転送後にGoogle Drive側のサイズとMD5を取得し、一致した場合のみ状態を原子的に保存して一時コピーを削除します。チェックサムが得られない場合は失敗とします。
- 同じソース・保存先・状態ディレクトリで再開すると、サイズと更新時刻が変わっていない成功済みファイルはスキップします。**スキップ時にクラウドを再検証しません。** 転送先を手動削除した場合や再検証したい場合は新しい `--work` を使ってください。
- 転送途中のファイルのバイト位置からの再開ではなく、ファイル単位の再試行です。
- `state.json` はパスと転送先・検証日時・チェックサムの記録です。保管してください。異なる保存先やライブラリには別のworkディレクトリを指定します。
- 同じworkディレクトリでの同時実行はロックで防ぎます。別workを使って同じ転送先へ同時実行しないでください。

## テスト

```sh
go test -race ./...
go vet ./...
```

テストではローカルの偽ライブラリと転送バックエンドを使い、成功後の再開、検証失敗、元ファイル保持、容量制限、宛先変更拒否、キャンセル等を検証します。実際のGoogleアカウントは使いません。
