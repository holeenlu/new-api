/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import fs from 'node:fs/promises'
import path from 'node:path'

const LOCALES_DIR = path.resolve('src/i18n/locales')

function stableStringify(obj) {
  return `${JSON.stringify(obj, null, 2)}\n`
}

const en = {
  'Automatic mode retries channels with the same non-empty tag and matching data policy. Channels without a tag stay isolated.':
    'Automatic mode retries channels with the same non-empty tag and matching data policy. Channels without a tag stay isolated.',
  'Subscription OAuth automatic mode retries compatible channels in the selected group. Channel tags are metadata only.':
    'Subscription OAuth automatic mode retries compatible channels in the selected group. Channel tags are metadata only.',
  'Capacity cycle times': 'Capacity cycle times',
  'Capacity wait limit (seconds)': 'Capacity wait limit (seconds)',
  'Disabled by default to avoid amplifying upstream rate limits':
    'Disabled by default to avoid amplifying upstream rate limits',
  'Maximum passes through channels in the same retry pool':
    'Maximum passes through channels in the same retry pool',
  'Maximum retryable failures per OAuth credential before same-group failover':
    'Maximum retryable failures per OAuth credential before same-group failover',
  'Retry 429 across OAuth accounts': 'Retry 429 across OAuth accounts',
  'Subscription OAuth retry': 'Subscription OAuth retry',
  'Total wait budget across all capacity cycles':
    'Total wait budget across all capacity cycles',
  'Upstream retry times': 'Upstream retry times',
  'Channel tag is required for tag isolation':
    'Channel tag is required for tag isolation',
  'All requests use the host location profile':
    'All requests use the host location profile',
  'All requests use the proxy egress profile':
    'All requests use the proxy egress profile',
  'Allow client location': 'Allow client location',
  'Automatic route selection': 'Automatic route selection',
  'Client location is allowed; client IP remains blocked':
    'Client location is allowed; client IP remains blocked',
  'Client location is removed': 'Client location is removed',
  Coordinates: 'Coordinates',
  'Effective selection rule': 'Effective selection rule',
  'Expected public IP': 'Expected public IP',
  'Force host profile': 'Force host profile',
  'Force proxy egress profile': 'Force proxy egress profile',
  'Host network profile': 'Host network profile',
  Location: 'Location',
  'Location forwarding mode': 'Location forwarding mode',
  'Network profiles refreshed': 'Network profiles refreshed',
  'Network profiles refreshed with warnings':
    'Network profiles refreshed with warnings',
  'Not configured': 'Not configured',
  'Failed to refresh network profiles': 'Failed to refresh network profiles',
  'Proxied channels use the proxy egress profile; direct channels use the host profile':
    'Proxied channels use the proxy egress profile; direct channels use the host profile',
  'Strip client location': 'Strip client location',
  'System VPN or TUN is active; all requests use the proxy egress profile':
    'System VPN or TUN is active; all requests use the proxy egress profile',
  'System-level VPN or TUN': 'System-level VPN or TUN',
  'Same channel tag': 'Same channel tag',
  Timezone: 'Timezone',
  'Upstream Privacy': 'Upstream Privacy',
  'VPN or proxy egress profile': 'VPN or proxy egress profile',
}

const newKeys = {
  en,
  zh: {
    'Automatic mode retries channels with the same non-empty tag and matching data policy. Channels without a tag stay isolated.':
      '自动模式仅在具有相同非空标签且数据策略一致的渠道间重试。未设置标签的渠道保持隔离。',
    'Subscription OAuth automatic mode retries compatible channels in the selected group. Channel tags are metadata only.':
      '订阅 OAuth 自动模式仅在已选分组内的兼容渠道间重试，渠道标签只作为管理元数据。',
    'Capacity cycle times': '容量循环次数',
    'Capacity wait limit (seconds)': '容量等待上限（秒）',
    'Disabled by default to avoid amplifying upstream rate limits':
      '默认关闭，避免放大上游限流',
    'Maximum passes through channels in the same retry pool':
      '同一重试池内渠道的最大循环轮数',
    'Maximum retryable failures per OAuth credential before same-group failover':
      '单个 OAuth 凭证触发同分组切换前允许的最大可重试失败次数',
    'Retry 429 across OAuth accounts': '跨 OAuth 账号重试 429',
    'Subscription OAuth retry': '订阅 OAuth 重试',
    'Total wait budget across all capacity cycles': '所有容量循环累计等待预算',
    'Upstream retry times': '上游重试次数',
    'Channel tag is required for tag isolation': '按标签隔离时必须填写渠道标签',
    'All requests use the host location profile':
      '所有请求使用宿主网络位置画像',
    'All requests use the proxy egress profile': '所有请求使用代理出口位置画像',
    'Allow client location': '允许客户端位置',
    'Automatic route selection': '自动选择网络路径',
    'Client location is allowed; client IP remains blocked':
      '允许客户端位置，但仍阻止客户端 IP',
    'Client location is removed': '客户端位置已删除',
    Coordinates: '经纬度',
    'Effective selection rule': '当前选择规则',
    'Expected public IP': '预期公网 IP',
    'Force host profile': '强制使用宿主画像',
    'Force proxy egress profile': '强制使用代理出口画像',
    'Host network profile': '宿主网络画像',
    Location: '位置',
    'Location forwarding mode': '位置转发模式',
    'Network profiles refreshed': '网络画像刷新成功',
    'Network profiles refreshed with warnings':
      '网络画像已刷新，但部分路径探测失败',
    'Not configured': '未配置',
    'Failed to refresh network profiles': '无法刷新网络画像',
    'Proxied channels use the proxy egress profile; direct channels use the host profile':
      '使用代理的渠道采用代理出口画像，直连渠道采用宿主画像',
    'Strip client location': '删除客户端位置',
    'System VPN or TUN is active; all requests use the proxy egress profile':
      '系统 VPN 或 TUN 已启用，所有请求使用代理出口画像',
    'System-level VPN or TUN': '系统级 VPN 或 TUN',
    'Same channel tag': '相同渠道标签',
    Timezone: '时区',
    'Upstream Privacy': '上游隐私',
    'VPN or proxy egress profile': 'VPN 或代理出口画像',
  },
  'zh-TW': {
    'Automatic mode retries channels with the same non-empty tag and matching data policy. Channels without a tag stay isolated.':
      '自動模式僅在具有相同非空標籤且資料策略一致的渠道間重試。未設定標籤的渠道保持隔離。',
    'Subscription OAuth automatic mode retries compatible channels in the selected group. Channel tags are metadata only.':
      '訂閱 OAuth 自動模式僅在已選分組內的相容渠道間重試，渠道標籤只作為管理中繼資料。',
    'Capacity cycle times': '容量循環次數',
    'Capacity wait limit (seconds)': '容量等待上限（秒）',
    'Disabled by default to avoid amplifying upstream rate limits':
      '預設關閉，避免放大上游限流',
    'Maximum passes through channels in the same retry pool':
      '同一重試池內渠道的最大循環輪數',
    'Maximum retryable failures per OAuth credential before same-group failover':
      '單一 OAuth 憑證觸發同分組切換前允許的最大可重試失敗次數',
    'Retry 429 across OAuth accounts': '跨 OAuth 帳號重試 429',
    'Subscription OAuth retry': '訂閱 OAuth 重試',
    'Total wait budget across all capacity cycles': '所有容量循環累計等待預算',
    'Upstream retry times': '上游重試次數',
    'Channel tag is required for tag isolation': '按標籤隔離時必須填寫渠道標籤',
    'Same channel tag': '相同渠道標籤',
    'All requests use the host location profile':
      '所有請求使用主機網路位置設定',
    'All requests use the proxy egress profile': '所有請求使用代理出口位置設定',
    'Allow client location': '允許用戶端位置',
    'Automatic route selection': '自動選擇網路路徑',
    'Client location is allowed; client IP remains blocked':
      '允許用戶端位置，但仍封鎖用戶端 IP',
    'Client location is removed': '用戶端位置已移除',
    Coordinates: '經緯度',
    'Effective selection rule': '目前選擇規則',
    'Expected public IP': '預期公網 IP',
    'Force host profile': '強制使用主機設定',
    'Force proxy egress profile': '強制使用代理出口設定',
    'Host network profile': '主機網路設定',
    Location: '位置',
    'Location forwarding mode': '位置轉送模式',
    'Network profiles refreshed': '網路設定已重新整理',
    'Network profiles refreshed with warnings':
      '網路設定已重新整理，但部分路徑偵測失敗',
    'Not configured': '未設定',
    'Failed to refresh network profiles': '無法重新整理網路設定',
    'Proxied channels use the proxy egress profile; direct channels use the host profile':
      '使用代理的渠道採用代理出口設定，直接連線渠道採用主機設定',
    'Strip client location': '移除用戶端位置',
    'System VPN or TUN is active; all requests use the proxy egress profile':
      '系統 VPN 或 TUN 已啟用，所有請求使用代理出口設定',
    'System-level VPN or TUN': '系統層級 VPN 或 TUN',
    Timezone: '時區',
    'Upstream Privacy': '上游隱私',
    'VPN or proxy egress profile': 'VPN 或代理出口設定',
  },
  fr: {
    'Automatic mode retries channels with the same non-empty tag and matching data policy. Channels without a tag stay isolated.':
      'Le mode automatique réessaie les canaux ayant la même étiquette non vide et la même politique de données. Les canaux sans étiquette restent isolés.',
    'Subscription OAuth automatic mode retries compatible channels in the selected group. Channel tags are metadata only.':
      'Le mode automatique OAuth par abonnement réessaie les canaux compatibles du groupe sélectionné. Les étiquettes de canal servent uniquement de métadonnées.',
    'Capacity cycle times': 'Cycles de capacité',
    'Capacity wait limit (seconds)': 'Limite d’attente de capacité (secondes)',
    'Disabled by default to avoid amplifying upstream rate limits':
      'Désactivé par défaut pour ne pas amplifier les limites en amont',
    'Maximum passes through channels in the same retry pool':
      'Nombre maximal de passages dans le même pool de nouvelle tentative',
    'Maximum retryable failures per OAuth credential before same-group failover':
      'Nombre maximal d’échecs réessayables par identifiant OAuth avant basculement dans le même groupe',
    'Retry 429 across OAuth accounts': 'Réessayer les 429 entre comptes OAuth',
    'Subscription OAuth retry': 'Nouvelle tentative OAuth d’abonnement',
    'Total wait budget across all capacity cycles':
      'Budget d’attente total pour tous les cycles de capacité',
    'Upstream retry times': 'Tentatives en amont',
    'Channel tag is required for tag isolation':
      'Une étiquette de canal est requise pour l’isolation par étiquette',
    'All requests use the host location profile':
      'Toutes les requêtes utilisent le profil d’emplacement de l’hôte',
    'All requests use the proxy egress profile':
      'Toutes les requêtes utilisent le profil de sortie du proxy',
    'Allow client location': 'Autoriser l’emplacement du client',
    'Automatic route selection': 'Sélection automatique de l’itinéraire',
    'Client location is allowed; client IP remains blocked':
      'L’emplacement du client est autorisé, mais son adresse IP reste bloquée',
    'Client location is removed': 'L’emplacement du client est supprimé',
    Coordinates: 'Coordonnées',
    'Effective selection rule': 'Règle de sélection effective',
    'Expected public IP': 'Adresse IP publique attendue',
    'Force host profile': 'Forcer le profil de l’hôte',
    'Force proxy egress profile': 'Forcer le profil de sortie du proxy',
    'Host network profile': 'Profil réseau de l’hôte',
    Location: 'Emplacement',
    'Location forwarding mode': 'Mode de transmission de l’emplacement',
    'Network profiles refreshed': 'Profils réseau actualisés',
    'Network profiles refreshed with warnings':
      'Profils réseau actualisés avec des avertissements',
    'Not configured': 'Non configuré',
    'Failed to refresh network profiles':
      'Impossible d’actualiser les profils réseau',
    'Proxied channels use the proxy egress profile; direct channels use the host profile':
      'Les canaux avec proxy utilisent le profil de sortie du proxy ; les canaux directs utilisent le profil de l’hôte',
    'Strip client location': 'Supprimer l’emplacement du client',
    'System VPN or TUN is active; all requests use the proxy egress profile':
      'Le VPN ou TUN système est actif ; toutes les requêtes utilisent le profil de sortie du proxy',
    'System-level VPN or TUN': 'VPN ou TUN système',
    'Same channel tag': 'Même étiquette de canal',
    Timezone: 'Fuseau horaire',
    'Upstream Privacy': 'Confidentialité en amont',
    'VPN or proxy egress profile': 'Profil de sortie VPN ou proxy',
  },
  ja: {
    'Automatic mode retries channels with the same non-empty tag and matching data policy. Channels without a tag stay isolated.':
      '自動モードでは、同じ空でないタグと一致するデータポリシーを持つチャネルのみ再試行します。タグのないチャネルは分離されたままです。',
    'Subscription OAuth automatic mode retries compatible channels in the selected group. Channel tags are metadata only.':
      'サブスクリプション OAuth の自動モードは、選択されたグループ内の互換チャネルで再試行します。チャネルタグは管理用メタデータのみです。',
    'Capacity cycle times': '容量サイクル回数',
    'Capacity wait limit (seconds)': '容量待機上限（秒）',
    'Disabled by default to avoid amplifying upstream rate limits':
      '上流のレート制限を増幅しないよう既定では無効です',
    'Maximum passes through channels in the same retry pool':
      '同じ再試行プール内の最大巡回回数',
    'Maximum retryable failures per OAuth credential before same-group failover':
      '同一グループ内で切り替える前の OAuth 認証情報ごとの再試行可能な最大失敗回数',
    'Retry 429 across OAuth accounts': 'OAuth アカウント間で 429 を再試行',
    'Subscription OAuth retry': 'サブスクリプション OAuth 再試行',
    'Total wait budget across all capacity cycles':
      '全容量サイクルの合計待機時間',
    'Upstream retry times': '上流再試行回数',
    'Channel tag is required for tag isolation':
      'タグ分離にはチャネルタグが必要です',
    'All requests use the host location profile':
      'すべてのリクエストでホスト位置プロファイルを使用します',
    'All requests use the proxy egress profile':
      'すべてのリクエストでプロキシ出口プロファイルを使用します',
    'Allow client location': 'クライアント位置を許可',
    'Automatic route selection': '経路を自動選択',
    'Client location is allowed; client IP remains blocked':
      'クライアント位置は許可されますが、クライアント IP は引き続き遮断されます',
    'Client location is removed': 'クライアント位置は削除されます',
    Coordinates: '緯度・経度',
    'Effective selection rule': '現在の選択ルール',
    'Expected public IP': '想定パブリック IP',
    'Force host profile': 'ホストプロファイルを強制',
    'Force proxy egress profile': 'プロキシ出口プロファイルを強制',
    'Host network profile': 'ホストネットワークプロファイル',
    Location: '場所',
    'Location forwarding mode': '位置転送モード',
    'Network profiles refreshed': 'ネットワークプロファイルを更新しました',
    'Network profiles refreshed with warnings':
      '警告付きでネットワークプロファイルを更新しました',
    'Not configured': '未設定',
    'Failed to refresh network profiles':
      'ネットワークプロファイルを更新できませんでした',
    'Proxied channels use the proxy egress profile; direct channels use the host profile':
      'プロキシ経由のチャネルはプロキシ出口プロファイルを、直接接続のチャネルはホストプロファイルを使用します',
    'Strip client location': 'クライアント位置を削除',
    'System VPN or TUN is active; all requests use the proxy egress profile':
      'システム VPN または TUN が有効なため、すべてのリクエストでプロキシ出口プロファイルを使用します',
    'System-level VPN or TUN': 'システムレベルの VPN または TUN',
    'Same channel tag': '同じチャネルタグ',
    Timezone: 'タイムゾーン',
    'Upstream Privacy': '上流プライバシー',
    'VPN or proxy egress profile': 'VPN またはプロキシ出口プロファイル',
  },
  ru: {
    'Automatic mode retries channels with the same non-empty tag and matching data policy. Channels without a tag stay isolated.':
      'В автоматическом режиме повтор выполняется только по каналам с одинаковым непустым тегом и совпадающей политикой данных. Каналы без тега остаются изолированными.',
    'Subscription OAuth automatic mode retries compatible channels in the selected group. Channel tags are metadata only.':
      'Автоматический режим OAuth-подписки повторяет запросы через совместимые каналы выбранной группы. Теги каналов используются только как метаданные.',
    'Capacity cycle times': 'Число циклов ёмкости',
    'Capacity wait limit (seconds)': 'Лимит ожидания ёмкости (секунды)',
    'Disabled by default to avoid amplifying upstream rate limits':
      'По умолчанию отключено, чтобы не усиливать ограничения поставщика',
    'Maximum passes through channels in the same retry pool':
      'Максимальное число проходов по каналам одного пула',
    'Maximum retryable failures per OAuth credential before same-group failover':
      'Максимум повторяемых сбоев для учетных данных OAuth до переключения в той же группе',
    'Retry 429 across OAuth accounts': 'Повторять 429 между OAuth-аккаунтами',
    'Subscription OAuth retry': 'Повторы OAuth-подписки',
    'Total wait budget across all capacity cycles':
      'Общий бюджет ожидания всех циклов ёмкости',
    'Upstream retry times': 'Число повторов поставщика',
    'Channel tag is required for tag isolation':
      'Для изоляции по тегу требуется тег канала',
    'All requests use the host location profile':
      'Все запросы используют профиль расположения хоста',
    'All requests use the proxy egress profile':
      'Все запросы используют профиль выхода прокси',
    'Allow client location': 'Разрешить местоположение клиента',
    'Automatic route selection': 'Автоматический выбор маршрута',
    'Client location is allowed; client IP remains blocked':
      'Местоположение клиента разрешено, но IP-адрес клиента по-прежнему блокируется',
    'Client location is removed': 'Местоположение клиента удаляется',
    Coordinates: 'Координаты',
    'Effective selection rule': 'Действующее правило выбора',
    'Expected public IP': 'Ожидаемый публичный IP-адрес',
    'Force host profile': 'Принудительно использовать профиль хоста',
    'Force proxy egress profile':
      'Принудительно использовать профиль выхода прокси',
    'Host network profile': 'Сетевой профиль хоста',
    Location: 'Местоположение',
    'Location forwarding mode': 'Режим передачи местоположения',
    'Network profiles refreshed': 'Сетевые профили обновлены',
    'Network profiles refreshed with warnings':
      'Сетевые профили обновлены с предупреждениями',
    'Not configured': 'Не настроено',
    'Failed to refresh network profiles': 'Не удалось обновить сетевые профили',
    'Proxied channels use the proxy egress profile; direct channels use the host profile':
      'Каналы с прокси используют профиль выхода прокси, а прямые каналы — профиль хоста',
    'Strip client location': 'Удалять местоположение клиента',
    'System VPN or TUN is active; all requests use the proxy egress profile':
      'Системный VPN или TUN активен; все запросы используют профиль выхода прокси',
    'System-level VPN or TUN': 'Системный VPN или TUN',
    'Same channel tag': 'Одинаковый тег канала',
    Timezone: 'Часовой пояс',
    'Upstream Privacy': 'Конфиденциальность вышестоящего соединения',
    'VPN or proxy egress profile': 'Профиль выхода VPN или прокси',
  },
  vi: {
    'Automatic mode retries channels with the same non-empty tag and matching data policy. Channels without a tag stay isolated.':
      'Chế độ tự động chỉ thử lại các kênh có cùng thẻ không rỗng và chính sách dữ liệu khớp nhau. Kênh không có thẻ vẫn được cô lập.',
    'Subscription OAuth automatic mode retries compatible channels in the selected group. Channel tags are metadata only.':
      'Chế độ tự động OAuth thuê bao thử lại qua các kênh tương thích trong nhóm đã chọn. Thẻ kênh chỉ là siêu dữ liệu quản trị.',
    'Capacity cycle times': 'Số vòng dung lượng',
    'Capacity wait limit (seconds)': 'Giới hạn chờ dung lượng (giây)',
    'Disabled by default to avoid amplifying upstream rate limits':
      'Mặc định tắt để tránh khuếch đại giới hạn phía thượng nguồn',
    'Maximum passes through channels in the same retry pool':
      'Số lượt tối đa qua các kênh trong cùng nhóm thử lại',
    'Maximum retryable failures per OAuth credential before same-group failover':
      'Số lỗi có thể thử lại tối đa trên mỗi thông tin xác thực OAuth trước khi chuyển trong cùng nhóm',
    'Retry 429 across OAuth accounts': 'Thử lại 429 giữa các tài khoản OAuth',
    'Subscription OAuth retry': 'Thử lại OAuth đăng ký',
    'Total wait budget across all capacity cycles':
      'Tổng ngân sách chờ cho mọi vòng dung lượng',
    'Upstream retry times': 'Số lần thử lại thượng nguồn',
    'Channel tag is required for tag isolation':
      'Cần có thẻ kênh để cô lập theo thẻ',
    'All requests use the host location profile':
      'Mọi yêu cầu dùng hồ sơ vị trí máy chủ',
    'All requests use the proxy egress profile':
      'Mọi yêu cầu dùng hồ sơ đầu ra proxy',
    'Allow client location': 'Cho phép vị trí máy khách',
    'Automatic route selection': 'Tự động chọn tuyến',
    'Client location is allowed; client IP remains blocked':
      'Vị trí máy khách được phép nhưng IP máy khách vẫn bị chặn',
    'Client location is removed': 'Vị trí máy khách bị loại bỏ',
    Coordinates: 'Tọa độ',
    'Effective selection rule': 'Quy tắc lựa chọn hiện tại',
    'Expected public IP': 'IP công khai dự kiến',
    'Force host profile': 'Buộc dùng hồ sơ máy chủ',
    'Force proxy egress profile': 'Buộc dùng hồ sơ đầu ra proxy',
    'Host network profile': 'Hồ sơ mạng máy chủ',
    Location: 'Vị trí',
    'Location forwarding mode': 'Chế độ chuyển tiếp vị trí',
    'Network profiles refreshed': 'Đã làm mới hồ sơ mạng',
    'Network profiles refreshed with warnings':
      'Đã làm mới hồ sơ mạng kèm cảnh báo',
    'Not configured': 'Chưa cấu hình',
    'Failed to refresh network profiles': 'Không thể làm mới hồ sơ mạng',
    'Proxied channels use the proxy egress profile; direct channels use the host profile':
      'Kênh qua proxy dùng hồ sơ đầu ra proxy; kênh trực tiếp dùng hồ sơ máy chủ',
    'Strip client location': 'Loại bỏ vị trí máy khách',
    'System VPN or TUN is active; all requests use the proxy egress profile':
      'VPN hoặc TUN hệ thống đang hoạt động; mọi yêu cầu dùng hồ sơ đầu ra proxy',
    'System-level VPN or TUN': 'VPN hoặc TUN cấp hệ thống',
    'Same channel tag': 'Cùng thẻ kênh',
    Timezone: 'Múi giờ',
    'Upstream Privacy': 'Quyền riêng tư phía thượng nguồn',
    'VPN or proxy egress profile': 'Hồ sơ đầu ra VPN hoặc proxy',
  },
}

const extraKeys = {
  en: {
    'After signing in, copy the full callback URL from the browser address bar and paste it here.':
      'After signing in, copy the full callback URL from the browser address bar and paste it here.',
    'After signing in, copy the full localhost callback URL from the browser address bar and paste it here.':
      'After signing in, copy the full localhost callback URL from the browser address bar and paste it here.',
    'OAuth account cannot access this resource. Check the subscription and account permissions.':
      'OAuth account cannot access this resource. Check the subscription and account permissions.',
    'OAuth start failed': 'OAuth start failed',
  },
  zh: {
    'After signing in, copy the full callback URL from the browser address bar and paste it here.':
      '登录后，请从浏览器地址栏复制完整的回调 URL 并粘贴到此处。',
    'After signing in, copy the full localhost callback URL from the browser address bar and paste it here.':
      '登录后，从浏览器地址栏复制完整的 localhost 回调 URL 并粘贴到此处。',
    'OAuth account cannot access this resource. Check the subscription and account permissions.':
      'OAuth 账户无权访问此资源。请检查订阅状态和账户权限。',
    'OAuth start failed': 'OAuth 授权启动失败',
  },
  'zh-TW': {
    'After signing in, copy the full callback URL from the browser address bar and paste it here.':
      '登入後，請從瀏覽器網址列複製完整的回呼 URL 並貼到此處。',
    'After signing in, copy the full localhost callback URL from the browser address bar and paste it here.':
      '登入後，請從瀏覽器網址列複製完整的 localhost 回呼 URL 並貼到此處。',
    'OAuth account cannot access this resource. Check the subscription and account permissions.':
      'OAuth 帳戶無權存取此資源。請檢查訂閱狀態和帳戶權限。',
    'OAuth start failed': 'OAuth 授權啟動失敗',
  },
  fr: {
    'After signing in, copy the full callback URL from the browser address bar and paste it here.':
      "Après la connexion, copiez l'URL de rappel complète depuis la barre d'adresse du navigateur et collez-la ici.",
    'After signing in, copy the full localhost callback URL from the browser address bar and paste it here.':
      'Après connexion, copiez l’URL de rappel localhost complète depuis la barre d’adresse et collez-la ici.',
    'OAuth account cannot access this resource. Check the subscription and account permissions.':
      "Le compte OAuth ne peut pas accéder à cette ressource. Vérifiez l'abonnement et les autorisations du compte.",
    'OAuth start failed': 'Échec du démarrage OAuth',
  },
  ja: {
    'After signing in, copy the full callback URL from the browser address bar and paste it here.':
      'ログイン後、ブラウザーのアドレスバーから完全なコールバック URL をコピーして、ここに貼り付けてください。',
    'After signing in, copy the full localhost callback URL from the browser address bar and paste it here.':
      'サインイン後、ブラウザのアドレスバーから localhost の完全なコールバック URL をコピーして貼り付けてください。',
    'OAuth account cannot access this resource. Check the subscription and account permissions.':
      'OAuth アカウントはこのリソースにアクセスできません。サブスクリプションとアカウント権限を確認してください。',
    'OAuth start failed': 'OAuth の開始に失敗しました',
  },
  ru: {
    'After signing in, copy the full callback URL from the browser address bar and paste it here.':
      'После входа скопируйте полный URL обратного вызова из адресной строки браузера и вставьте его сюда.',
    'After signing in, copy the full localhost callback URL from the browser address bar and paste it here.':
      'После входа скопируйте полный URL обратного вызова localhost из адресной строки браузера и вставьте его сюда.',
    'OAuth account cannot access this resource. Check the subscription and account permissions.':
      'Учетная запись OAuth не имеет доступа к этому ресурсу. Проверьте подписку и разрешения учетной записи.',
    'OAuth start failed': 'Не удалось запустить OAuth',
  },
  vi: {
    'After signing in, copy the full callback URL from the browser address bar and paste it here.':
      'Sau khi đăng nhập, hãy sao chép URL callback đầy đủ từ thanh địa chỉ của trình duyệt và dán vào đây.',
    'After signing in, copy the full localhost callback URL from the browser address bar and paste it here.':
      'Sau khi đăng nhập, sao chép URL callback localhost đầy đủ từ thanh địa chỉ trình duyệt và dán vào đây.',
    'OAuth account cannot access this resource. Check the subscription and account permissions.':
      'Tài khoản OAuth không thể truy cập tài nguyên này. Hãy kiểm tra gói đăng ký và quyền của tài khoản.',
    'OAuth start failed': 'Không thể khởi động OAuth',
  },
}

for (const [locale, entries] of Object.entries(extraKeys)) {
  newKeys[locale] = {
    ...newKeys[locale],
    ...entries,
  }
}

const removedKeys = [
  'Maximum retryable failures per OAuth credential before same-tag failover',
]

async function main() {
  for (const [locale, trans] of Object.entries(newKeys)) {
    const filePath = path.join(LOCALES_DIR, `${locale}.json`)
    const json = JSON.parse(await fs.readFile(filePath, 'utf8'))

    for (const key of removedKeys) {
      delete json.translation[key]
    }
    for (const [key, value] of Object.entries(trans)) {
      json.translation[key] = value
    }
    json.translation = Object.fromEntries(
      Object.entries(json.translation).sort(([a], [b]) => a.localeCompare(b))
    )
    await fs.writeFile(filePath, stableStringify(json), 'utf8')
  }
}

await main()
