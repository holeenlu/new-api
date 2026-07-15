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
import i18next from 'i18next'

export function getChannelErrorMessage(
  errorCode: string | undefined,
  fallback: string
): string {
  switch (errorCode) {
    case 'oauth_unauthorized':
      return i18next.t(
        'OAuth credential is invalid or expired. Reauthorize or refresh the channel credential.'
      )
    case 'oauth_forbidden':
      return i18next.t(
        'OAuth account cannot access this resource. Check the subscription and account permissions.'
      )
    case 'model_not_supported':
      return i18next.t(
        'This model is not available to the OAuth account. Fetch the upstream model list or choose another model.'
      )
    default:
      return fallback
  }
}
