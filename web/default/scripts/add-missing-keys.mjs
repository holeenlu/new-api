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
  Timezone: 'Timezone',
  'Upstream Privacy': 'Upstream Privacy',
  'VPN or proxy egress profile': 'VPN or proxy egress profile',
}

const newKeys = {
  en,
  zh: {
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
    Timezone: '时区',
    'Upstream Privacy': '上游隐私',
    'VPN or proxy egress profile': 'VPN 或代理出口画像',
  },
  'zh-TW': {
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
    Timezone: 'Fuseau horaire',
    'Upstream Privacy': 'Confidentialité en amont',
    'VPN or proxy egress profile': 'Profil de sortie VPN ou proxy',
  },
  ja: {
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
    Timezone: 'タイムゾーン',
    'Upstream Privacy': '上流プライバシー',
    'VPN or proxy egress profile': 'VPN またはプロキシ出口プロファイル',
  },
  ru: {
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
    Timezone: 'Часовой пояс',
    'Upstream Privacy': 'Конфиденциальность вышестоящего соединения',
    'VPN or proxy egress profile': 'Профиль выхода VPN или прокси',
  },
  vi: {
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
    Timezone: 'Múi giờ',
    'Upstream Privacy': 'Quyền riêng tư phía thượng nguồn',
    'VPN or proxy egress profile': 'Hồ sơ đầu ra VPN hoặc proxy',
  },
}

async function main() {
  for (const [locale, trans] of Object.entries(newKeys)) {
    const filePath = path.join(LOCALES_DIR, `${locale}.json`)
    const json = JSON.parse(await fs.readFile(filePath, 'utf8'))
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
